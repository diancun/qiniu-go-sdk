package operation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/qiniupd/qiniu-go-sdk/api.v8/dot"
	"github.com/qiniupd/qiniu-go-sdk/api.v8/kodocli"
)

type APIName = dot.APIName
type DotType = dot.DotType

const (
	SDKDotType    DotType = dot.SDKDotType
	HTTPDotType   DotType = dot.HTTPDotType
	APINameV1Stat APIName = "monitor_v1_stat"
)

type Dotter struct {
	accessKey         string
	secretKey         string
	bucket            string
	bufferRecordsLock sync.Mutex
	bufferRecords     []*localDotRecord
	bufferFile        *os.File
	dotSelector       *HostSelector
	interval          time.Duration
	uploadedAt        time.Time
	maxBufferSize     int64
	uploadTries       int
}

var dotClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   500 * time.Millisecond,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	Timeout: 1 * time.Second,
}

func NewDotter(config *Config) (dotter *Dotter, err error) {
	if len(config.MonitorHosts) == 0 {
		return
	}
	dotFilePath := filepath.Join(cacheDirectory, "dot-file")
	dotFile, err := os.OpenFile(dotFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	dotter = &Dotter{
		accessKey:     config.Ak,
		secretKey:     config.Sk,
		bucket:        config.Bucket,
		bufferFile:    dotFile,
		dotSelector:   NewHostSelector(dupStrings(config.MonitorHosts), nil, 0, time.Duration(config.PunishTimeS)*time.Second, 0, -1, shouldRetry),
		interval:      time.Duration(config.DotIntervalS) * time.Second,
		maxBufferSize: int64(config.MaxDotBufferSize),
		uploadTries:   config.Retry,
		uploadedAt:    time.Now(),
	}
	if dotter.uploadTries <= 0 {
		dotter.uploadTries = 10
	}
	if dotter.interval <= 0 {
		dotter.interval = 10 * time.Second
	}
	if dotter.maxBufferSize <= 0 {
		dotter.maxBufferSize = 1 << 20
	}
	return
}

type localDotRecord struct {
	DotType DotType `json:"t"`
	APIName APIName `json:"a"`
	Failed  bool    `json:"f,omitempty"`
}

func (dotter *Dotter) Dot(dotType DotType, apiName APIName, success bool) (err error) {
	if dotter == nil {
		return
	}

	dotter.bufferRecordsLock.Lock()
	defer dotter.bufferRecordsLock.Unlock()

	dotter.bufferRecords = append(dotter.bufferRecords, &localDotRecord{
		DotType: dotType,
		APIName: apiName,
		Failed:  !success,
	})

	lockFile, err := dotter.tryLockFile()
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = nil
		}
		return
	}
	defer dotter.unlockFile(lockFile)

	for _, bufRecord := range dotter.bufferRecords {
		if err = json.NewEncoder(dotter.bufferFile).Encode(bufRecord); err != nil {
			return
		}
	}
	dotter.bufferRecords = dotter.bufferRecords[0:0]

	err = dotter.tryUploadAsync()
	return
}

type remoteDotRecord struct {
	Type         DotType `json:"type"`
	APIName      APIName `json:"api_name"`
	SuccessCount uint64  `json:"success_count"`
	FailedCount  uint64  `json:"failed_count"`
}

type remoteDotRecords struct {
	Records []*remoteDotRecord `json:"logs"`
}

func (dotter *Dotter) tryUploadAsync() (err error) {
	c, err := dotter.timeToUpload()
	if err != nil {
		return
	}
	if c {
		go dotter.upload()
	}
	return
}

func (dotter *Dotter) upload() (err error) {
	return dotter.retry(func(host string) (dontRetryOrRewardOrPunish bool, err error) {
		makeRequestBody := func() (body io.Reader, err error) {
			c, err := dotter.timeToUpload()
			if err != nil {
				return
			}
			if !c {
				return
			}

			dotFilePath := filepath.Join(cacheDirectory, "dot-file")
			dotFile, err := os.Open(dotFilePath)
			if err != nil {
				return
			}
			defer dotFile.Close()

			var records remoteDotRecords
			decoder := json.NewDecoder(dotFile)
			for {
				var r localDotRecord
				if err = decoder.Decode(&r); err != nil {
					break
				}
				var pRecord *remoteDotRecord = nil
				for _, record := range records.Records {
					if record.APIName == r.APIName && record.Type == r.DotType {
						pRecord = record
					}
				}
				if pRecord == nil {
					pRecord = &remoteDotRecord{Type: r.DotType, APIName: r.APIName}
					records.Records = append(records.Records, pRecord)
				}
				if r.Failed {
					pRecord.FailedCount += 1
				} else {
					pRecord.SuccessCount += 1
				}
			}
			if errors.Is(err, io.EOF) {
				err = nil
			} else {
				return
			}

			if len(records.Records) == 0 {
				return
			}
			uploadData, err := json.Marshal(records)
			if err != nil {
				return
			}
			body = bytes.NewReader(uploadData)
			return
		}

		lockFile, err := dotter.tryLockFile()
		if err != nil {
			dontRetryOrRewardOrPunish = true
			if errors.Is(err, syscall.EWOULDBLOCK) {
				err = nil
			}
			return
		}
		defer dotter.unlockFile(lockFile)

		reqBody, err := makeRequestBody()
		if err != nil {
			dontRetryOrRewardOrPunish = true
			return
		} else if reqBody == nil {
			dontRetryOrRewardOrPunish = true
			return
		}

		req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1/stat", host), reqBody)
		if err != nil {
			go dotter.Dot(HTTPDotType, APINameV1Stat, false)
			return
		}
		req.Header.Add("Content-Type", "application/json")
		req.Header.Add("Authorization", "UpToken "+kodocli.MakeAuthTokenString(dotter.accessKey, dotter.secretKey, &kodocli.AuthPolicy{
			Scope:    dotter.bucket,
			Deadline: time.Now().Add(10 * time.Second).Unix(),
		}))

		resp, err := dotClient.Do(req)
		if err != nil {
			go dotter.Dot(HTTPDotType, APINameV1Stat, false)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode/100 != 2 {
			go dotter.Dot(HTTPDotType, APINameV1Stat, false)
			err = fmt.Errorf("monitor dot status code error: %d", resp.StatusCode)
			return
		}

		go dotter.Dot(HTTPDotType, APINameV1Stat, true)
		if err = dotter.bufferFile.Truncate(0); err != nil {
			dontRetryOrRewardOrPunish = true
		}
		return
	})
}

func (dotter *Dotter) retry(f func(host string) (bool, error)) (err error) {
	var dontRetryOrRewardOrPunish bool
	for i := 0; i < dotter.uploadTries; i++ {
		host := dotter.dotSelector.SelectHost()
		dontRetryOrRewardOrPunish, err = f(host)
		if err != nil {
			if !dontRetryOrRewardOrPunish {
				elog.Warn("monitor try failed. punish host", host, i, err)
				dotter.dotSelector.PunishIfNeeded(host, err)
			}
			if !dontRetryOrRewardOrPunish && shouldRetry(err) {
				continue
			}
		} else if !dontRetryOrRewardOrPunish {
			dotter.dotSelector.Reward(host)
		}
		break
	}
	return
}

func (dotter *Dotter) timeToUpload() (bool, error) {
	fileInfo, err := dotter.bufferFile.Stat()
	if err != nil {
		return false, err
	}
	c := fileInfo.Size() >= dotter.maxBufferSize || dotter.uploadedAt.Add(dotter.interval).Before(time.Now())
	return c, nil
}

func (dotter *Dotter) tryLockFile() (*os.File, error) {
	dotFileLockPath := filepath.Join(cacheDirectory, "dot-file.lock")
	file, err := os.OpenFile(dotFileLockPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return file, err
}

func (dotter *Dotter) unlockFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if err != nil {
		return err
	}
	return file.Close()
}
