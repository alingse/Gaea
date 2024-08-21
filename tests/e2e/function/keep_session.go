// Copyright 2024 The Gaea Authors. All Rights Reserved.
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

package function

import (
	"fmt"
	"time"

	"github.com/XiaoMi/Gaea/tests/e2e/config"
	"github.com/XiaoMi/Gaea/tests/e2e/util"

	"github.com/onsi/ginkgo/v2"
)

// This Ginkgo test suite is designed to validate session persistence and transaction handling in Gaea when interfacing with a MySQL database in a dual-slave configuration. It specifically tests the behavior of both XA (eXtended Architecture) transactions and regular transactions under different session settings.
// Test scenarios are designed to evaluate:
// 1. **XA Transactions**: These tests verify that Gaea can handle XA transactions, which are designed for distributed transaction systems. The tests cover starting, preparing, and committing XA transactions, and they ensure that data manipulated within these transactions is correctly committed to the database.
// 2. **Error Handling in Transactions**: These tests ensure that Gaea appropriately reports errors when transaction commands are misused or when session settings do not support the executed operations.
// 3. **Regular Transactions**: Testing regular transaction commands (`BEGIN`, `COMMIT`) to confirm that Gaea handles these consistently within the context of session persistence.
// 4. **Session Persistence**: Some tests specifically manipulate the session persistence settings to observe how Gaea handles transaction states when sessions are not maintained.
// This suite is crucial for ensuring that Gaea reliably manages transactions across multiple database instances and maintains correct session states, which is essential for applications that require high reliability and strict data consistency in distributed environments.
var _ = ginkgo.Describe("keep session test", func() {
	e2eMgr := config.NewE2eManager()
	db := config.DefaultE2eDatabase
	slice := e2eMgr.NsSlices[config.SliceDualSlave]
	table := config.DefaultE2eTable
	// namespace prepare
	initNs, err := config.ParseNamespaceTmpl(config.DefaultNamespaceTmpl, slice)
	util.ExpectNoError(err, "parse namespace template")

	ginkgo.BeforeEach(func() {
		// mysql prepare
		masterAdminConn, err := slice.GetMasterAdminConn(0)
		util.ExpectNoError(err)
		err = util.SetupDatabaseAndInsertData(masterAdminConn, db, table)
		util.ExpectNoError(err)
		err = e2eMgr.ModifyNamespace(initNs)
		util.ExpectNoError(err)
		// wait mysql data  sync and namespace load
		time.Sleep(500 * time.Millisecond)
	})

	ginkgo.Context("test xa transaction stmts", func() {
		ginkgo.It("should handle xa transactions and normal transactions correctly", func() {
			sqlCases := []struct {
				TestSQLs    []string
				CheckSQL    string
				ExpectRes   [][]string
				ExpectErr   string
				KeepSession bool
			}{
				{
					TestSQLs: []string{`xa start "xa_test1"`, fmt.Sprintf(`insert into %s.%s values(1001, "xa_test1")`, db, table),
						`xa end "xa_test1"`, `xa prepare "xa_test1"`, `xa commit "xa_test1"`},
					CheckSQL: fmt.Sprintf("SELECT * FROM %s.%s WHERE id = %d limit 1", db, table, 1001),
					ExpectRes: [][]string{{
						"1001", "xa_test1",
					}},
					KeepSession: true,
				},
				{
					TestSQLs: []string{`xa start "xa_test2"`, fmt.Sprintf(`insert into %s.%s values(1002, "xa_test2")`, db, table),
						`xa end "xa_test2"`, `xa commit "xa_test2"`},
					ExpectErr:   "Error 1399: XAER_RMFAIL: The command cannot be executed when global transaction is in the  IDLE state",
					KeepSession: true,
				},
				{
					TestSQLs: []string{`begin`, fmt.Sprintf(`insert into %s.%s values(1003, "tx_test3")`, db, table), `commit`},
					CheckSQL: fmt.Sprintf("SELECT * FROM %s.%s WHERE id = %d limit 1", db, table, 1003),
					ExpectRes: [][]string{{
						"1003", "tx_test3",
					}},
					KeepSession: true,
				},
				{
					TestSQLs:    []string{`xa start "xa_test4"`, `xa end "xa_test4"`},
					ExpectErr:   "Error 1399: XAER_RMFAIL: The command cannot be executed when global transaction is in the  NON-EXISTING state",
					KeepSession: false,
				},
			}

			for _, sqlCase := range sqlCases {
				// set keep session config
				initNs.SetForKeepSession = sqlCase.KeepSession
				err = e2eMgr.ModifyNamespace(initNs)
				util.ExpectNoError(err)
				gaeaTestConn, err := e2eMgr.GetReadWriteGaeaUserConn()
				util.ExpectNoError(err)
				for _, testSql := range sqlCase.TestSQLs {
					_, err = gaeaTestConn.Exec(testSql)
				}

				if sqlCase.ExpectErr != "" {
					if sqlCase.ExpectErr != err.Error() {
						fmt.Printf("sql exec error: %s\n", err.Error())
						util.ExpectNoError(err)
					}
					continue
				}

				gaeaCheckConn, err := e2eMgr.GetReadWriteGaeaUserConn()
				util.ExpectNoError(err)
				err = checkFunc(gaeaCheckConn, sqlCase.CheckSQL, sqlCase.ExpectRes)
				util.ExpectNoError(err)
			}
		})
	})

	ginkgo.AfterEach(func() {
		e2eMgr.Clean()
	})
})
