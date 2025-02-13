package operation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/qiniupd/qiniu-go-sdk/api.v7/auth/qbox"
	"github.com/qiniupd/qiniu-go-sdk/api.v8/kodo"
	q "github.com/qiniupd/qiniu-go-sdk/api.v8/kodocli"
)

// 上传器
type Uploader struct {
	config                Configurable
	singleClusterUploader *singleClusterUploader
}

// 根据配置创建上传器
func NewUploader(c *Config) *Uploader {
	return &Uploader{config: c, singleClusterUploader: newSingleClusterUploader(c)}
}

// 根据环境变量创建上传器
func NewUploaderV2() *Uploader {
	c := getCurrentConfigurable()
	if c == nil {
		return nil
	} else if singleClusterConfig, ok := c.(*Config); ok {
		return NewUploader(singleClusterConfig)
	} else {
		return &Uploader{config: c}
	}
}

// 上传内存数据到指定对象中
func (p *Uploader) UploadData(data []byte, key string) (err error) {
	if p.singleClusterUploader != nil {
		return p.singleClusterUploader.uploadData(data, key)
	}
	if config, exists := p.config.forKey(key); !exists {
		return ErrUndefinedConfig
	} else {
		return newSingleClusterUploader(config).uploadData(data, key)
	}
}

// 从 Reader 中阅读指定大小的数据并上传到指定对象中
func (p *Uploader) UploadDataReader(data io.ReaderAt, size int, key string) (err error) {
	if p.singleClusterUploader != nil {
		return p.singleClusterUploader.uploadDataReader(data, size, key)
	}
	if config, exists := p.config.forKey(key); !exists {
		return ErrUndefinedConfig
	} else {
		return newSingleClusterUploader(config).uploadDataReader(data, size, key)
	}
}

// 上传指定文件到指定对象中
func (p *Uploader) Upload(file string, key string) (err error) {
	if p.singleClusterUploader != nil {
		return p.singleClusterUploader.upload(file, key)
	}
	if config, exists := p.config.forKey(key); !exists {
		return ErrUndefinedConfig
	} else {
		return newSingleClusterUploader(config).upload(file, key)
	}
}

// 从 Reader 中阅读全部数据并上传到指定对象中
func (p *Uploader) UploadReader(reader io.Reader, key string) (err error) {
	if p.singleClusterUploader != nil {
		return p.singleClusterUploader.uploadReader(reader, key)
	}
	if config, exists := p.config.forKey(key); !exists {
		return ErrUndefinedConfig
	} else {
		return newSingleClusterUploader(config).uploadReader(reader, key)
	}
}

type singleClusterUploader struct {
	bucket        string
	upHosts       []string
	credentials   *qbox.Mac
	partSize      int64
	upConcurrency int
	queryer       *Queryer
}

func newSingleClusterUploader(c *Config) *singleClusterUploader {
	mac := qbox.NewMac(c.Ak, c.Sk)
	part := c.PartSize * 1024 * 1024
	if part < 4*1024*1024 {
		part = 4 * 1024 * 1024
	}
	var queryer *Queryer = nil

	if len(c.UcHosts) > 0 {
		queryer = NewQueryer(c)
	}

	return &singleClusterUploader{
		bucket:        c.Bucket,
		upHosts:       dupStrings(c.UpHosts),
		credentials:   mac,
		partSize:      part,
		upConcurrency: c.UpConcurrency,
		queryer:       queryer,
	}
}

func (p *singleClusterUploader) makeUptoken(policy *kodo.PutPolicy) string {
	var rr = *policy
	if rr.Expires == 0 {
		rr.Expires = 3600 + uint32(time.Now().Unix())
	}
	b, _ := json.Marshal(&rr)
	return qbox.SignWithData(p.credentials, b)
}

func (p *singleClusterUploader) uploadData(data []byte, key string) (err error) {
	t := time.Now()
	defer func() {
		elog.Info("up time ", key, time.Now().Sub(t))
	}()
	key = strings.TrimPrefix(key, "/")
	policy := kodo.PutPolicy{
		Scope:   p.bucket + ":" + key,
		Expires: 3600*24 + uint32(time.Now().Unix()),
	}

	upToken := p.makeUptoken(&policy)

	upHosts := p.upHosts
	if p.queryer != nil {
		if hosts := p.queryer.QueryUpHosts(false); len(hosts) > 0 {
			upHosts = hosts
		}
	}

	var uploader = q.NewUploader(1, &q.UploadConfig{
		UpHosts:        upHosts,
		UploadPartSize: p.partSize,
		Concurrency:    p.upConcurrency,
	})
	for i := 0; i < 3; i++ {
		err = uploader.Put2(context.Background(), nil, upToken, key, bytes.NewReader(data), int64(len(data)), nil)
		if err == nil {
			break
		}
		elog.Info("small upload retry", i, err)
	}
	return
}

func (p *singleClusterUploader) uploadDataReader(data io.ReaderAt, size int, key string) (err error) {
	t := time.Now()
	defer func() {
		elog.Info("up time ", key, time.Now().Sub(t))
	}()
	key = strings.TrimPrefix(key, "/")
	policy := kodo.PutPolicy{
		Scope:   p.bucket + ":" + key,
		Expires: 3600*24 + uint32(time.Now().Unix()),
	}

	upToken := p.makeUptoken(&policy)

	upHosts := p.upHosts
	if p.queryer != nil {
		if hosts := p.queryer.QueryUpHosts(false); len(hosts) > 0 {
			upHosts = hosts
		}
	}

	var uploader = q.NewUploader(1, &q.UploadConfig{
		UpHosts:        upHosts,
		UploadPartSize: p.partSize,
		Concurrency:    p.upConcurrency,
	})

	for i := 0; i < 3; i++ {
		err = uploader.Put2(context.Background(), nil, upToken, key, newReaderAtNopCloser(data), int64(size), nil)
		if err == nil {
			break
		}
		elog.Info("small upload retry", i, err)
	}
	return
}

func (p *singleClusterUploader) upload(file string, key string) (err error) {
	t := time.Now()
	defer func() {
		elog.Info("up time ", key, time.Now().Sub(t))
	}()
	key = strings.TrimPrefix(key, "/")
	policy := kodo.PutPolicy{
		Scope:   p.bucket + ":" + key,
		Expires: 3600*24 + uint32(time.Now().Unix()),
	}
	upToken := p.makeUptoken(&policy)

	f, err := os.Open(file)
	if err != nil {
		elog.Info("open file failed: ", file, err)
		return err
	}
	defer f.Close()

	fInfo, err := f.Stat()
	if err != nil {
		elog.Info("get file stat failed: ", err)
		return err
	}

	upHosts := p.upHosts
	if p.queryer != nil {
		if hosts := p.queryer.QueryUpHosts(false); len(hosts) > 0 {
			upHosts = hosts
		}
	}

	var uploader = q.NewUploader(1, &q.UploadConfig{
		UpHosts:        upHosts,
		UploadPartSize: p.partSize,
		Concurrency:    p.upConcurrency,
	})

	if fInfo.Size() <= p.partSize {
		for i := 0; i < 3; i++ {
			err = uploader.Put2(context.Background(), nil, upToken, key, newReaderAtNopCloser(f), fInfo.Size(), nil)
			if err == nil {
				break
			}
			elog.Info("small upload retry", i, err)
		}
		return
	}

	for i := 0; i < 3; i++ {
		err = uploader.Upload(context.Background(), nil, upToken, key, newReaderAtNopCloser(f), fInfo.Size(), nil,
			func(partIdx int, etag string) {
				elog.Info("callback", partIdx, etag)
			})
		if err == nil {
			break
		}
		elog.Info("part upload retry", i, err)
	}
	return
}

func (p *singleClusterUploader) uploadReader(reader io.Reader, key string) (err error) {
	t := time.Now()
	defer func() {
		elog.Info("up time ", key, time.Now().Sub(t))
	}()
	key = strings.TrimPrefix(key, "/")
	policy := kodo.PutPolicy{
		Scope:   p.bucket + ":" + key,
		Expires: 3600*24 + uint32(time.Now().Unix()),
	}
	upToken := p.makeUptoken(&policy)

	upHosts := p.upHosts
	if p.queryer != nil {
		if hosts := p.queryer.QueryUpHosts(false); len(hosts) > 0 {
			upHosts = hosts
		}
	}

	var uploader = q.NewUploader(1, &q.UploadConfig{
		UpHosts:        upHosts,
		UploadPartSize: p.partSize,
		Concurrency:    p.upConcurrency,
	})

	bufReader := bufio.NewReader(reader)
	firstPart, err := ioutil.ReadAll(io.LimitReader(bufReader, p.partSize))
	if err != nil {
		return
	}

	smallUpload := false
	if len(firstPart) < int(p.partSize) {
		smallUpload = true
	} else if _, err = bufReader.Peek(1); err != nil {
		if err == io.EOF {
			smallUpload = true
		} else {
			return err
		}
	}

	if smallUpload {
		for i := 0; i < 3; i++ {
			err = uploader.Put2(context.Background(), nil, upToken, key, bytes.NewReader(firstPart), int64(len(firstPart)), nil)
			if err == nil {
				break
			}
			elog.Info("small upload retry", i, err)
		}
		return
	}

	err = uploader.StreamUpload(context.Background(), nil, upToken, key, io.MultiReader(bytes.NewReader(firstPart), bufReader),
		func(partIdx int, etag string) {
			elog.Info("callback", partIdx, etag)
		})
	return err
}

type readerAtCloser interface {
	io.ReaderAt
	io.Closer
}

type readerAtNopCloser struct {
	io.ReaderAt
}

func (readerAtNopCloser) Close() error { return nil }

// newReaderAtNopCloser returns a readerAtCloser with a no-op Close method wrapping
// the provided ReaderAt r.
func newReaderAtNopCloser(r io.ReaderAt) readerAtCloser {
	return readerAtNopCloser{r}
}
