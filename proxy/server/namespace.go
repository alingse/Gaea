// Copyright 2019 The Gaea Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/XiaoMi/Gaea/backend"
	"github.com/XiaoMi/Gaea/log"
	"github.com/XiaoMi/Gaea/models"
	"github.com/XiaoMi/Gaea/mysql"
	"github.com/XiaoMi/Gaea/proxy/plan"
	"github.com/XiaoMi/Gaea/proxy/router"
	"github.com/XiaoMi/Gaea/proxy/sequence"
	"github.com/XiaoMi/Gaea/util"
	"github.com/XiaoMi/Gaea/util/cache"
)

const (
	namespaceDelayClose = 60
)

const (
	defaultSQLCacheCapacity  = 64
	defaultPlanCacheCapacity = 128

	defaultSlowSQLTime       = 1000  // millisecond
	defaultMaxSqlExecuteTime = 0     // 默认为0，不开启慢sql熔断功能
	defaultMaxSqlResultSize  = 10000 // 默认为10000, 限制查询返回的结果集大小不超过该阈值
	defaultTimeAfterNoAlive  = 8     // 如果发现slave状态异常，则每sleep 1，2，4，8秒再检查， 8s之后
	// 认为Slave已下线，如果需要快速判定状态，可减少该值
	defaultMaxClientConnections = 100000000 //Big enough

)

// UserProperty means runtime user properties
type UserProperty struct {
	RWFlag        int
	RWSplit       int
	OtherProperty int
}

// Namespace is struct driected used by server
type Namespace struct {
	name                string
	allowedDBs          map[string]bool
	defaultPhyDBs       map[string]string // logicDBName-phyDBName
	sqls                map[string]string //key: sql fingerprint
	slowSQLTime         int64             // session slow sql time, millisecond, default 1000
	allowips            []util.IPInfo
	router              *router.Router
	sequences           *sequence.SequenceManager
	slices              map[string]*backend.Slice // key: slice name
	userProperties      map[string]*UserProperty  // key: user name ,value: user's properties
	defaultCharset      string
	defaultCollationID  mysql.CollationID
	openGeneralLog      bool
	maxSqlExecuteTime   int // session max sql execute time,millisecond
	maxSqlResultSize    int
	defaultSlice        string
	downAfterNoAlive    int
	secondsBehindMaster uint64

	slowSQLCache         *cache.LRUCache
	errorSQLCache        *cache.LRUCache
	backendSlowSQLCache  *cache.LRUCache
	backendErrorSQLCache *cache.LRUCache
	planCache            *cache.LRUCache

	maxClientConnections int
	CheckSelectLock      bool
}

// DumpToJSON  means easy encode json
func (n *Namespace) DumpToJSON() []byte {
	return models.JSONEncode(n)
}

// NewNamespace init namespace
func NewNamespace(namespaceConfig *models.Namespace) (*Namespace, error) {
	var err error
	namespace := &Namespace{
		name:                 namespaceConfig.Name,
		sqls:                 make(map[string]string, 16),
		userProperties:       make(map[string]*UserProperty, 2),
		openGeneralLog:       namespaceConfig.OpenGeneralLog,
		slowSQLCache:         cache.NewLRUCache(defaultSQLCacheCapacity),
		errorSQLCache:        cache.NewLRUCache(defaultSQLCacheCapacity),
		backendSlowSQLCache:  cache.NewLRUCache(defaultSQLCacheCapacity),
		backendErrorSQLCache: cache.NewLRUCache(defaultSQLCacheCapacity),
		planCache:            cache.NewLRUCache(defaultPlanCacheCapacity),
		defaultSlice:         namespaceConfig.DefaultSlice,
	}

	defer func() {
		if err != nil {
			namespace.Close(false)
		}
	}()

	// init black sql
	namespace.sqls = parseBlackSqls(namespaceConfig.BlackSQL)

	// init session slow sql time
	namespace.slowSQLTime, err = parseSlowSQLTime(namespaceConfig.SlowSQLTime)
	if err != nil {
		return nil, fmt.Errorf("parse slowSQLTime error: %v", err)
	}

	// init session slow sql max execute time
	if namespaceConfig.MaxSqlExecuteTime <= 0 {
		namespace.maxSqlExecuteTime = defaultMaxSqlExecuteTime
	} else {
		namespace.maxSqlExecuteTime = namespaceConfig.MaxSqlExecuteTime
	}

	// init session slow sql max result size
	if namespaceConfig.MaxSqlResultSize <= 0 && namespaceConfig.MaxSqlResultSize != -1 {
		namespace.maxSqlResultSize = defaultMaxSqlResultSize
	} else {
		namespace.maxSqlResultSize = namespaceConfig.MaxSqlResultSize
	}

	allowDBs := make(map[string]bool, len(namespaceConfig.AllowedDBS))
	for db, allowed := range namespaceConfig.AllowedDBS {
		allowDBs[strings.TrimSpace(db)] = allowed
	}
	namespace.allowedDBs = allowDBs

	defaultPhyDBs := make(map[string]string, len(namespaceConfig.DefaultPhyDBS))
	for db, phyDB := range namespaceConfig.DefaultPhyDBS {
		defaultPhyDBs[strings.TrimSpace(db)] = strings.TrimSpace(phyDB)
	}

	namespace.defaultPhyDBs, err = parseDefaultPhyDB(defaultPhyDBs, allowDBs)
	if err != nil {
		return nil, fmt.Errorf("parse defaultPhyDBs error: %v", err)
	}

	// init allow ip
	allowips, err := parseAllowIps(namespaceConfig.AllowedIP)
	if err != nil {
		return nil, fmt.Errorf("parse allowips error: %v", err)
	}
	namespace.allowips = allowips

	namespace.defaultCharset, namespace.defaultCollationID, err = parseCharset(namespaceConfig.DefaultCharset, namespaceConfig.DefaultCollation)
	if err != nil {
		return nil, fmt.Errorf("parse charset error: %v", err)
	}

	// init user properties
	for _, user := range namespaceConfig.Users {
		up := &UserProperty{RWFlag: user.RWFlag, RWSplit: user.RWSplit, OtherProperty: user.OtherProperty}
		namespace.userProperties[user.UserName] = up
	}

	// init backend slices
	namespace.slices, err = parseSlices(namespaceConfig.Slices, namespace.defaultCharset, namespace.defaultCollationID)
	if err != nil {
		return nil, fmt.Errorf("init slices of namespace: %s failed, err: %v", namespaceConfig.Name, err)
	}

	//Check slice master and slave status and mark them as unavailable when detect down
	if err = checkSlicesStatus(namespace.slices, namespace); err != nil {
		return nil, fmt.Errorf("check salve in slices of namespace: %s failed, err: %v", namespaceConfig.Name, err)
	}

	// init router
	namespace.router, err = router.NewRouter(namespaceConfig)
	if err != nil {
		return nil, fmt.Errorf("init router of namespace: %s failed, err: %v", namespace.name, err)
	}

	// init global sequences config
	// 目前只支持基于mysql的序列号
	sequences := sequence.NewSequenceManager()
	for _, v := range namespaceConfig.GlobalSequences {
		globalSequenceSlice, ok := namespace.slices[v.SliceName]
		if !ok {
			return nil, fmt.Errorf("init global sequence error: slice not found, sequence: %v", v)
		}
		seqName := strings.ToUpper(v.DB) + "." + strings.ToUpper(v.Table)
		seq := sequence.NewMySQLSequence(globalSequenceSlice, seqName, v.PKName)
		sequences.SetSequence(v.DB, v.Table, seq)
	}
	namespace.sequences = sequences

	if namespace.maxClientConnections <= 0 {
		namespace.maxClientConnections = defaultMaxClientConnections
	} else {
		namespace.maxClientConnections = namespaceConfig.MaxClientConnections
	}

	namespace.downAfterNoAlive = namespaceConfig.DownAfterNoAlive
	if namespace.downAfterNoAlive < 0 {
		return nil, fmt.Errorf("downAfterNoAlive should be greater than 0")
	}
	// not configurable yet, use default
	if namespace.downAfterNoAlive == 0 {
		namespace.downAfterNoAlive = defaultTimeAfterNoAlive
	}

	namespace.secondsBehindMaster = namespaceConfig.SecondsBehindMaster
	namespace.CheckSelectLock = namespaceConfig.CheckSelectLock

	return namespace, nil
}

// GetName return namespace of namespace
func (n *Namespace) GetName() string {
	return n.name
}

// GetSlice return slice of namespace
func (n *Namespace) GetSlice(name string) *backend.Slice {
	return n.slices[name]
}

// GetRouter return router of namespace
func (n *Namespace) GetRouter() *router.Router {
	return n.router
}

func (n *Namespace) GetSequences() *sequence.SequenceManager {
	return n.sequences
}

// IsClientIPAllowed check ip
func (n *Namespace) IsClientIPAllowed(clientIP net.IP) bool {
	if len(n.allowips) == 0 {
		return true
	}
	for _, ip := range n.allowips {
		if ip.Match(clientIP) {
			return true
		}
	}
	return false
}

func (n *Namespace) getSessionSlowSQLTime() int64 {
	return n.slowSQLTime
}

// IsAllowWrite check if user allow to write
func (n *Namespace) IsAllowWrite(user string) bool {
	return n.userProperties[user].RWFlag == models.ReadWrite
}

// IsRWSplit chekc if read write split
func (n *Namespace) IsRWSplit(user string) bool {
	return n.userProperties[user].RWSplit == models.ReadWriteSplit
}

// IsStatisticUser check if user is used to statistic
func (n *Namespace) IsStatisticUser(user string) bool {
	return n.userProperties[user].OtherProperty == models.StatisticUser
}

// GetUserProperty return user information
func (n *Namespace) GetUserProperty(user string) int {
	return n.userProperties[user].OtherProperty
}

func (n *Namespace) GetMaxExecuteTime() int {
	return n.maxSqlExecuteTime
}

func (n *Namespace) GetMaxResultSize() int {
	return n.maxSqlResultSize
}

// IsSQLAllowed check black sql
func (n *Namespace) IsSQLAllowed(reqCtx *util.RequestContext, sql string) bool {
	if len(n.sqls) == 0 {
		return true
	}

	fingerprint := mysql.GetFingerprint(sql)
	reqCtx.Set("fingerprint", fingerprint)
	md5 := mysql.GetMd5(fingerprint)
	if _, ok := n.sqls[md5]; ok {
		return false
	}

	return true
}

// IsAllowedDB if allowed database
func (n *Namespace) IsAllowedDB(dbname string) bool {
	allowed, ok := n.allowedDBs[dbname]
	return ok && allowed
}

// GetAllowedDBs return all allowed databases
func (n *Namespace) GetAllowedDBs() []string {
	var ret []string
	for db := range n.allowedDBs {
		ret = append(ret, db)
	}
	return ret
}

// GetDefaultPhyDB return default real database
func (n *Namespace) GetDefaultPhyDB(dbname string) (string, error) {
	if dbname == "" {
		return "", nil
	}
	phyDB, ok := n.defaultPhyDBs[dbname]
	if !ok {
		return "", fmt.Errorf("invalid db %s", dbname)
	}
	return phyDB, nil
}

func (n *Namespace) GetPhysicalDBs() map[string]string {
	return n.defaultPhyDBs
}

// GetDefaultCharset return default charset
func (n *Namespace) GetDefaultCharset() string {
	return n.defaultCharset
}

func (n *Namespace) GetDefaultSlice() string {
	return n.defaultSlice
}

// GetDefaultCollationID return default collation id
func (n *Namespace) GetDefaultCollationID() mysql.CollationID {
	return n.defaultCollationID
}

// GetCachedPlan get plan in cache
func (n *Namespace) GetCachedPlan(db, sql string) (plan.Plan, bool) {
	v, ok := n.planCache.Get(db + "|" + sql)
	if !ok {
		return nil, false
	}
	return v.(plan.Plan), true
}

// SetCachedPlan set plan in cache
func (n *Namespace) SetCachedPlan(db, sql string, p plan.Plan) {
	n.planCache.SetIfAbsent(db+"|"+sql, p)
}

// SetSlowSQLFingerprint store slow sql fingerprint
func (n *Namespace) SetSlowSQLFingerprint(md5, fingerprint string) {
	n.slowSQLCache.Set(md5, cache.CachedString(fingerprint))
}

// GetSlowSQLFingerprint return slow sql fingerprint
func (n *Namespace) GetSlowSQLFingerprint(md5 string) (string, bool) {
	v, ok := n.slowSQLCache.Get(md5)
	if !ok {
		return "", false
	}
	return string(v.(cache.CachedString)), true
}

// GetSlowSQLFingerprints return slow sql fingerprints
func (n *Namespace) GetSlowSQLFingerprints() map[string]string {
	ret := make(map[string]string)
	items := n.slowSQLCache.Items()
	for _, item := range items {
		ret[item.Key] = string(item.Value.(cache.CachedString))
	}
	return ret
}

// ClearSlowSQLFingerprints clear all slow sql fingerprints
func (n *Namespace) ClearSlowSQLFingerprints() {
	n.slowSQLCache.Clear()
}

// SetErrorSQLFingerprint store error sql fingerprint
func (n *Namespace) SetErrorSQLFingerprint(md5, fingerprint string) {
	n.errorSQLCache.Set(md5, cache.CachedString(fingerprint))
}

// GetErrorSQLFingerprint return error sql fingerprint
func (n *Namespace) GetErrorSQLFingerprint(md5 string) (string, bool) {
	v, ok := n.errorSQLCache.Get(md5)
	if !ok {
		return "", false
	}
	return string(v.(cache.CachedString)), true
}

// GetErrorSQLFingerprints return all error sql fingerprints
func (n *Namespace) GetErrorSQLFingerprints() map[string]string {
	ret := make(map[string]string)
	items := n.errorSQLCache.Items()
	for _, item := range items {
		ret[item.Key] = string(item.Value.(cache.CachedString))
	}
	return ret
}

// ClearErrorSQLFingerprints clear all error sql fingerprints
func (n *Namespace) ClearErrorSQLFingerprints() {
	n.errorSQLCache.Clear()
}

// SetBackendSlowSQLFingerprint store backend slow sql fingerprint
func (n *Namespace) SetBackendSlowSQLFingerprint(md5, fingerprint string) {
	n.backendSlowSQLCache.Set(md5, cache.CachedString(fingerprint))
}

// GetBackendSlowSQLFingerprint return backend slow sql fingerprint
func (n *Namespace) GetBackendSlowSQLFingerprint(md5 string) (string, bool) {
	v, ok := n.backendSlowSQLCache.Get(md5)
	if !ok {
		return "", false
	}
	return string(v.(cache.CachedString)), true
}

// GetBackendSlowSQLFingerprints return all backend slow sql fingerprints
func (n *Namespace) GetBackendSlowSQLFingerprints() map[string]string {
	ret := make(map[string]string)
	items := n.backendSlowSQLCache.Items()
	for _, item := range items {
		ret[item.Key] = string(item.Value.(cache.CachedString))
	}
	return ret
}

// ClearBackendSlowSQLFingerprints clear all backend slow sql fingerprints
func (n *Namespace) ClearBackendSlowSQLFingerprints() {
	n.backendSlowSQLCache.Clear()
}

// SetBackendErrorSQLFingerprint store backend error sql fingerprint
func (n *Namespace) SetBackendErrorSQLFingerprint(md5, fingerprint string) {
	n.backendErrorSQLCache.Set(md5, cache.CachedString(fingerprint))
}

// GetBackendErrorSQLFingerprint return backedn error sql fingerprint
func (n *Namespace) GetBackendErrorSQLFingerprint(md5 string) (string, bool) {
	v, ok := n.backendErrorSQLCache.Get(md5)
	if !ok {
		return "", false
	}
	return string(v.(cache.CachedString)), true
}

// GetBackendErrorSQLFingerprints return all backend error sql fingerprints
func (n *Namespace) GetBackendErrorSQLFingerprints() map[string]string {
	ret := make(map[string]string)
	items := n.backendErrorSQLCache.Items()
	for _, item := range items {
		ret[item.Key] = string(item.Value.(cache.CachedString))
	}
	return ret
}

// ClearBackendErrorSQLFingerprints clear all backend error sql fingerprints
func (n *Namespace) ClearBackendErrorSQLFingerprints() {
	n.backendErrorSQLCache.Clear()
}

// Close recycle resources of namespace
func (n *Namespace) Close(delay bool) {
	var err error
	// delay close time
	if delay {
		time.Sleep(time.Second * namespaceDelayClose)
	}
	for k := range n.slices {
		err = n.slices[k].Close()
		if err != nil {
			log.Warn("delay close slice: %s failed, err: %v", k, err)
			continue
		}
	}
	n.slowSQLCache.Clear()
	n.errorSQLCache.Clear()
	n.backendSlowSQLCache.Clear()
	n.backendErrorSQLCache.Clear()
}

func parseSlice(cfg *models.Slice, charset string, collationID mysql.CollationID) (*backend.Slice, error) {
	var err error
	s := new(backend.Slice)
	s.Cfg = *cfg
	s.SetCharsetInfo(charset, collationID)

	// parse master
	err = s.ParseMaster(cfg.Master)
	if err != nil {
		return nil, err
	}

	// parse slaves
	slaveInfo, err := s.ParseSlave(cfg.Slaves)
	if err != nil {
		return nil, err
	}
	s.Slave = slaveInfo

	// parse statistic slaves
	statisticSalve, err := s.ParseSlave(cfg.StatisticSlaves)
	if err != nil {
		return nil, err
	}
	s.StatisticSlave = statisticSalve
	return s, nil
}

func parseSlices(cfgSlices []*models.Slice, charset string, collationID mysql.CollationID) (map[string]*backend.Slice, error) {
	slices := make(map[string]*backend.Slice, len(cfgSlices))
	for _, v := range cfgSlices {
		v.Name = strings.TrimSpace(v.Name) // modify origin slice name, trim space
		if _, ok := slices[v.Name]; ok {
			return nil, fmt.Errorf("duplicate slice [%s]", v.Name)
		}

		s, err := parseSlice(v, charset, collationID)
		if err != nil {
			return nil, err
		}

		slices[v.Name] = s
	}

	return slices, nil
}

func checkSlicesStatus(slices map[string]*backend.Slice, namespace *Namespace) error {
	ctx, cancel := context.WithCancel(context.TODO())

	defer func() {
		if err := recover(); err != nil {
			cancel()
			log.Notice("find error: %s ...", err)
		}
	}()

	for _, v := range slices {
		if err := doCheckSlice(v, namespace, ctx); err != nil {
			cancel()
			log.Fatal("fail to start check salve liveness, namespace: %s, slice: %s", namespace, v.Cfg.Name)
			return err
		}
	}

	return nil
}

func shouldCheckSlaveDataSyncStatus(namespace *Namespace, crruentStatus backend.StatusCode, slice *backend.Slice, isMaster bool) bool {
	if isMaster || crruentStatus == backend.UP {
		return false
	}

	// AS when master is down, slave io thread will stop, we should not check slave sync crruentStatus, or we
	// will mark slave as down mistakely
	if masterStatus, _ := slice.Master.StatusMap.Load(0); masterStatus == backend.DOWN {
		return false
	}

	return namespace.secondsBehindMaster != 0
}

func doCheckSlice(slice *backend.Slice, namespace *Namespace, ctx context.Context) error {
	checkStatus := func(slaveInfo *backend.DBInfo, isMaster bool) {
		role := "Slave"
		if isMaster {
			role = "Master"
		}

		for {
			select {
			case <-ctx.Done():
				_ = log.Fatal("cancel by parent ......")
				return
			default:
				for idx, v := range slaveInfo.ConnPool {
					_ = log.Debug("namespace: %s, slice: %s, start to check %s %s by auto check...",
						namespace.name,
						role,
						slice.Cfg.Name,
						v.Addr())

					now := time.Now()

					status, conn := getInstanceStatus(namespace, v, ctx)
					// status is ok && this is slave && seconds_behind_master is not 0, we start to check master and slave lag
					// Pay attention!!!!, if master is down, slave IO thread is close, so we should skip check slave when master is down
					if shouldCheckSlaveDataSyncStatus(namespace, status, slice, isMaster) {
						if lag, _ := slaveIsLagBehand(conn, namespace); lag {
							status = backend.DOWN
						}
					}

					if conn != nil {
						conn.Recycle()
					}

					slaveInfo.StatusMap.Store(idx, status)

					logValue := fmt.Sprintf("namespace: %s, slice: %s, IP:PORT:[%s] is find %s by auto check..., take = %d ms",
						namespace.name,
						slice.Cfg.Name,
						v.Addr(),
						status.String(),
						time.Since(now).Milliseconds())
					if status == backend.DOWN {
						_ = log.Warn(logValue)
					} else {
						_ = log.Debug(logValue)
					}

				}
			}

			//every 2 second to check
			time.Sleep(2 * time.Second)
		}
	}

	go checkStatus(slice.Slave, false)
	go checkStatus(slice.StatisticSlave, false)
	go checkStatus(slice.Master, true)

	return nil
}

func slaveIsLagBehand(conn backend.PooledConnect, namespace *Namespace) (bool, error) {
	var slaveStatus SlaveStatus
	var err error
	if slaveStatus, err = GetSlaveStatus(conn); err != nil {
		_ = log.Warn("slave %s get SlaveStatus failed for %v", conn.GetAddr(), err)
		return false, err
	}

	if slaveStatus.SecondsBehindMaster > namespace.secondsBehindMaster {
		_ = log.Warn("slave %s SecondsBehindMaster(%d) is greater than %d", conn.GetAddr(), slaveStatus.SecondsBehindMaster,
			namespace.secondsBehindMaster)
		return true, nil
	}

	if slaveStatus.SlaveIORunning != "Yes" {
		_ = log.Warn("slave %s Slave_IO_Running(%s) is not Yes", conn.GetAddr(), slaveStatus.SlaveIORunning)
		return true, nil
	}
	if slaveStatus.SlaveSQLRunning != "Yes" {
		_ = log.Warn("slave %s SlaveSQLRunning(%s) is not Yes", conn.GetAddr(), slaveStatus.SlaveSQLRunning)
		return true, nil
	}

	return false, nil
}

func getInstanceStatus(namespace *Namespace, v backend.ConnectionPool, ctx context.Context) (backend.StatusCode, backend.PooledConnect) {
	var sleepTime = 1
	var err error
	var conn backend.PooledConnect
	for ; sleepTime < namespace.downAfterNoAlive; sleepTime *= 2 {
		conn, err = v.Get(ctx)
		if err != nil {
			if conn != nil {
				conn.Recycle()
				conn = nil
			}
			time.Sleep(time.Duration(sleepTime) * time.Second)
			continue
		}

		if err = conn.Ping(); err == nil {
			break
		}
		if err = conn.Reconnect(); err == nil {
			break
		}

		if conn != nil {
			conn.Recycle()
		}
		time.Sleep(time.Duration(sleepTime) * time.Second)
	}

	status := backend.UP
	if err != nil || sleepTime >= namespace.downAfterNoAlive {
		status = backend.DOWN
	}

	return status, conn
}

type SlaveStatus struct {
	SecondsBehindMaster uint64
	SlaveIORunning      string
	SlaveSQLRunning     string
	MasterLogFile       string
	ReadMasterLogPos    uint64
	RelayMasterLogFile  string
	ExecMasterLogPos    uint64
}

func GetSlaveStatus(conn backend.PooledConnect) (SlaveStatus, error) {
	var slaveStatus SlaveStatus
	res, err := conn.Execute("show slave status;", 0)
	if err != nil {
		return slaveStatus, err
	}

	for _, f := range res.Fields {
		fieldName := string(f.Name)
		var col interface{}
		col, err = res.GetValueByName(0, fieldName)
		if err != nil {
			_ = log.Warn("get field name Get '%s' failed in SlaveStatus, err: %v", fieldName, err)
			break
		}

		switch strings.ToLower(fieldName) {
		case "seconds_behind_master":
			switch col.(type) {
			case uint64:
				slaveStatus.SecondsBehindMaster = col.(uint64)
			default:
				slaveStatus.SecondsBehindMaster = 0
			}
		case "slave_io_running":
			switch col.(type) {
			case string:
				slaveStatus.SlaveIORunning = col.(string)
			default:
				slaveStatus.SlaveIORunning = "No"
			}
		case "slave_sql_running":
			switch col.(type) {
			case string:
				slaveStatus.SlaveSQLRunning = col.(string)
			default:
				slaveStatus.SlaveSQLRunning = "No"
			}
		case "master_log_file":
			switch col.(type) {
			case string:
				slaveStatus.MasterLogFile = col.(string)
			default:
				slaveStatus.MasterLogFile = ""
			}
		case "read_master_log_pos":
			switch col.(type) {
			case uint64:
				slaveStatus.ReadMasterLogPos = col.(uint64)
			default:
				slaveStatus.ReadMasterLogPos = 0
			}
		case "relay_master_log_file":
			switch col.(type) {
			case string:
				slaveStatus.RelayMasterLogFile = col.(string)
			default:
				slaveStatus.RelayMasterLogFile = ""
			}
		case "exec_master_log_pos":
			switch col.(type) {
			case uint64:
				slaveStatus.ExecMasterLogPos = col.(uint64)
			default:
				slaveStatus.ExecMasterLogPos = 0
			}
		default:
			continue
		}
	}
	return slaveStatus, err
}

func parseAllowIps(allowedIP []string) ([]util.IPInfo, error) {
	var allowips []util.IPInfo
	for _, ipStr := range allowedIP {
		ipStr = strings.TrimSpace(ipStr)
		if len(ipStr) == 0 {
			continue
		}
		ipInfo, err := util.ParseIPInfo(ipStr)
		if err != nil {
			return nil, err
		}
		allowips = append(allowips, ipInfo)
	}
	return allowips, nil
}

func parseBlackSqls(sqls []string) map[string]string {
	sqlMap := make(map[string]string, 10)
	for _, sql := range sqls {
		sql = strings.TrimSpace(sql)
		if len(sql) == 0 {
			continue
		}
		fingerprint := mysql.GetFingerprint(sql)
		md5 := mysql.GetMd5(fingerprint)
		sqlMap[md5] = fingerprint
	}
	return sqlMap
}

func parseSlowSQLTime(str string) (int64, error) {
	if str == "" {
		return defaultSlowSQLTime, nil
	}
	t, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return 0, err
	}
	if t < 0 {
		return 0, fmt.Errorf("less than zero")
	}

	return t, nil
}

func parseCharset(charset, collation string) (string, mysql.CollationID, error) {
	if charset == "" && collation == "" {
		return mysql.DefaultCharset, mysql.DefaultCollationID, nil
	}

	if collation == "" {
		collationID, ok := mysql.CharsetIds[charset]
		if !ok {
			return "", 0, errors.New("invalid charset")
		}
		return charset, collationID, nil
	}

	if err := mysql.VerifyCharset(charset, collation); err != nil {
		return "", 0, err
	}
	collationID, ok := mysql.CollationNames[collation]
	if !ok {
		return "", 0, errors.New("invalid collation")
	}

	return charset, collationID, nil
}

func parseDefaultPhyDB(defaultPhyDBs map[string]string, allowedDBs map[string]bool) (map[string]string, error) {
	// no logic database mode
	if len(defaultPhyDBs) == 0 {
		result := make(map[string]string, len(allowedDBs))
		for db := range allowedDBs {
			result[db] = db
		}
		return result, nil
	}

	// logic database mode
	for db := range allowedDBs {
		if _, ok := defaultPhyDBs[db]; !ok {
			return nil, fmt.Errorf("db %s have no phy db", db)
		}
	}
	return defaultPhyDBs, nil
}
