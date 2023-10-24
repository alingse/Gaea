package util

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"time"
)

type LogEntry struct {
	Timestamp      string
	Namespace      string
	User           string
	ClientAddr     string
	BackendAddr    string
	ConnectionID   int
	Query          string
	ResponseTimeMs float64
}

// CompareTimeStrings 比较两个时间字符串的大小
// 返回值为-1，0或1。-1表示time1 < time2，0表示time1 = time2，1表示time1 > time2
func CompareTimeStrings(time1 string, time2 string) (int, error) {
	// 解析时间字符串
	t1, err1 := time.Parse("2006-01-02 15:04:05.999", time1)
	t2, err2 := time.Parse("2006-01-02 15:04:05.999", time2)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("解析错误：%v %v", err1, err2)
	}

	// 比较时间
	if t1.Before(t2) {
		return -1, nil
	}
	if t1.After(t2) {
		return 1, nil
	}
	return 0, nil
}

func ReadLog(filepath string, searchString string, startTime string) ([]LogEntry, error) {
	// 打开文件
	file, err := os.Open(filepath)
	if err != nil {
		return []LogEntry{}, fmt.Errorf("open file:%s error %v ", filepath, err)
	}
	defer file.Close()

	// 创建一个新的Scanner
	scanner := bufio.NewScanner(file)

	// 正则表达式
	re := regexp.MustCompile(`\[(.*?)\] \[NOTICE\] \[(\d+)\] OK - (\d+\.\d+)ms - ns=(.*?), (.*?)@(.*?)->(.*?), mysql_connect_id=(\d+), r=\d+\|(.*?)$`)
	var logEntryRes []LogEntry
	for scanner.Scan() {
		line := scanner.Text()
		// 使用正则表达式匹配日志行
		matches := re.FindStringSubmatch(line)
		if len(matches) != 10 {
			continue
		}
		// 解析并填充结构体
		logEntry := LogEntry{}
		logEntry.Timestamp = matches[1]
		// 检查时间是否在startTime之后

		res, err := CompareTimeStrings(startTime, logEntry.Timestamp)
		if err != nil {
			return []LogEntry{}, nil
		}
		if res != -1 {
			continue
		}
		fmt.Sscanf(matches[3], "%f", &logEntry.ResponseTimeMs)
		logEntry.Namespace = matches[4]
		logEntry.User = matches[5]
		logEntry.ClientAddr = matches[6]
		logEntry.BackendAddr = matches[7]
		fmt.Sscanf(matches[8], "%d", &logEntry.ConnectionID)
		logEntry.Query = matches[9]

		if strings.Compare(searchString, logEntry.Query) != 0 {
			continue
		}
		logEntryRes = append(logEntryRes, logEntry)
	}

	if err := scanner.Err(); err != nil {
		return logEntryRes, fmt.Errorf("error during file scanning:%v", err)
	}
	return logEntryRes, nil
}

func RemoveLog(directory string) error {
	// 检查目录是否存在
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		// 如果目录不存在，则创建目录
		err := os.MkdirAll(directory, 0755)
		if err != nil {
			return err
		}
	}
	files, err := ioutil.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, file := range files {
		if !file.IsDir() {
			err := os.Remove(directory + "/" + file.Name())
			if err != nil {
				return err
			}
		}
	}
	return nil
}
