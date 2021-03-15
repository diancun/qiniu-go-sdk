package operation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiniupd/qiniu-go-sdk/api.v8/auth/qbox"
	"github.com/qiniupd/qiniu-go-sdk/api.v8/dot"
)

const (
	APINameGetFile            APIName = "io_getfile"
	APINameDownloadFile       APIName = "download_file"
	APINameDownloadReader     APIName = "download_reader"
	APINameDownloadBytes      APIName = "download_bytes"
	APINameDownloadRangeBytes APIName = "download_range_bytes"
)

type Downloader struct {
	bucket         string
	ioSelector     *HostSelector
	dotter         *Dotter
	credentials    *qbox.Mac
	queryer        *Queryer
	tries          int
	downloadClient *http.Client
}

func NewDownloader(c *Config) *Downloader {
	mac := qbox.NewMac(c.Ak, c.Sk)
	dotter, _ := NewDotter(c)
	downloadClient := &http.Client{
		Transport: newTransport(time.Duration(c.DialTimeoutMs)*time.Millisecond, 10*time.Minute),
		Timeout:   10 * time.Minute,
	}

	downloader := Downloader{
		bucket:         c.Bucket,
		credentials:    mac,
		queryer:        NewQueryer(c),
		tries:          c.Retry,
		downloadClient: downloadClient,
		dotter:         dotter,
	}

	update := func() []string {
		if downloader.queryer != nil {
			return downloader.queryer.QueryIoHosts(false)
		}
		return nil
	}
	downloader.ioSelector = NewHostSelector(dupStrings(c.IoHosts), update, 0, time.Duration(c.PunishTimeS)*time.Second, 0, -1, shouldRetry)

	if downloader.tries <= 0 {
		downloader.tries = 5
	}

	return &downloader
}

func NewDownloaderV2() *Downloader {
	c := getConf()
	if c == nil {
		return nil
	}
	return NewDownloader(c)
}

func (d *Downloader) withDot(apiName dot.APIName, f func() error) (err error) {
	err = f()
	d.dotter.Dot(dot.SDKDotType, apiName, err == nil)
	return
}

func (d *Downloader) retry(f func(host string) error) (err error) {
	for i := 0; i < d.tries; i++ {
		host := d.ioSelector.SelectHost()
		err = f(host)
		if err != nil {
			if d.ioSelector.PunishIfNeeded(host, err) {
				elog.Warn("download try failed. punish host", host, i, err)
				d.dotter.Dot(dot.HTTPDotType, APINameGetFile, false)
			} else {
				elog.Warn("download try failed but not punish host", host, i, err)
				d.dotter.Dot(dot.HTTPDotType, APINameGetFile, true)
			}
			if shouldRetry(err) {
				continue
			}
		} else {
			d.ioSelector.Reward(host)
			d.dotter.Dot(dot.HTTPDotType, APINameGetFile, true)
		}
		break
	}
	return
}

func (d *Downloader) DownloadFile(key, path string) (*os.File, error) {
	return d.DownloadFileWithContext(context.Background(), key, path)
}

func (d *Downloader) DownloadFileWithContext(ctx context.Context, key, path string) (*os.File, error) {
	var f *os.File
	err := d.withDot(APINameDownloadFile, func() error {
		return d.retry(func(host string) error {
			var err error
			f, err = d.downloadFileInner(ctx, host, key, path)
			return err
		})
	})
	return f, err
}

func (d *Downloader) DownloadReader(key string) (io.ReadCloser, error) {
	return d.DownloadReaderWithContext(context.Background(), key)
}

func (d *Downloader) DownloadReaderWithContext(ctx context.Context, key string) (io.ReadCloser, error) {
	var r io.ReadCloser
	err := d.withDot(APINameDownloadReader, func() error {
		return d.retry(func(host string) error {
			var err error
			r, err = d.downloadReaderInner(ctx, host, key)
			return err
		})
	})
	return r, err
}

func (d *Downloader) DownloadBytes(key string) ([]byte, error) {
	return d.DownloadBytesWithContext(context.Background(), key)
}

func (d *Downloader) DownloadBytesWithContext(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := d.withDot(APINameDownloadBytes, func() error {
		return d.retry(func(host string) error {
			var err error
			data, err = d.downloadBytesInner(ctx, host, key)
			return err
		})
	})
	return data, err
}

func (d *Downloader) DownloadRangeBytes(key string, offset, size int64) (int64, []byte, error) {
	return d.DownloadRangeBytesWithContext(context.Background(), key, offset, size)
}

func (d *Downloader) DownloadRangeBytesWithContext(ctx context.Context, key string, offset, size int64) (int64, []byte, error) {
	var (
		l    int64
		data []byte
	)
	err := d.withDot(APINameDownloadRangeBytes, func() error {
		return d.retry(func(host string) error {
			var err error
			l, data, err = d.downloadRangeBytesInner(ctx, host, key, offset, size)
			return err
		})
	})
	return l, data, err
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func (d *Downloader) downloadFileInner(ctx context.Context, host, key, path string) (*os.File, error) {
	if strings.HasPrefix(key, "/") {
		key = strings.TrimPrefix(key, "/")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	length, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	elog.Debug("downloadFileInner with remote path", key)
	url := fmt.Sprintf("%s/getfile/%s/%s/%s", host, d.credentials.AccessKey, d.bucket, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "")
	if length != 0 {
		r := fmt.Sprintf("bytes=%d-", length)
		req.Header.Set("Range", r)
		elog.Info("continue download", key, "Range", r)
	}

	response, err := d.downloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return f, nil
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		if response.Body != nil {
			response.Body.Close()
		}
		return nil, errors.New(response.Status)
	}
	ctLength := response.ContentLength
	n, err := io.Copy(f, response.Body)
	if err != nil {
		return nil, err
	}
	if ctLength != n {
		elog.Warn("download", key, "length not equal with ctlength:", ctLength, "actual:", n)
	}
	f.Seek(0, io.SeekStart)
	return f, nil
}

func (d *Downloader) downloadReaderInner(ctx context.Context, host, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "/") {
		key = strings.TrimPrefix(key, "/")
	}

	elog.Debug("downloadReaderInner with remote path", key)
	url := fmt.Sprintf("%s/getfile/%s/%s/%s", host, d.credentials.AccessKey, d.bucket, url.PathEscape(key))
	reader := urlReader{
		url:    url,
		ctx:    ctx,
		client: d.downloadClient,
		dotter: d.dotter,
		tries:  d.tries,
	}
	if err := reader.sendRequest(); err != nil {
		return nil, err
	} else {
		return &reader, nil
	}
}

type urlReader struct {
	url      string
	ctx      context.Context
	client   *http.Client
	dotter   *Dotter
	response *http.Response
	closed   bool
	offset   int
	tries    int
}

func (r *urlReader) Read(p []byte) (n int, err error) {
	if r.closed {
		n, err = 0, io.EOF
		return
	}
	for i := 0; i < r.tries; i++ {
		if r.response == nil {
			if err = r.sendRequest(); err != nil {
				return
			}
		}
		if r.response.Body == nil {
			n, err = 0, io.EOF
			return
		}
		n, err = r.response.Body.Read(p)
		if i == r.tries-1 { // Last Retry
			r.offset += n
		}
		if err == nil || err == io.EOF {
			return
		}
		r.response.Body.Close()
		r.response = nil
	}
	return
}

func (r *urlReader) sendRequest() (err error) {
	req, err := http.NewRequestWithContext(r.ctx, "GET", r.url, http.NoBody)
	if err != nil {
		return
	}
	req.Header.Set("Accept-Encoding", "")
	if r.offset != 0 {
		rangeHeader := fmt.Sprintf("bytes=%d-", r.offset)
		req.Header.Set("Range", rangeHeader)
		elog.Info("continue download:", r.url, "from:", r.offset)
	}

	r.response, err = r.client.Do(req)
	if err != nil {
		r.dotter.Dot(dot.HTTPDotType, APINameGetFile, false)
		return
	}
	if r.response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		r.dotter.Dot(dot.HTTPDotType, APINameGetFile, true)
		return
	}
	if r.response.StatusCode != http.StatusOK && r.response.StatusCode != http.StatusPartialContent {
		if r.response.Body != nil {
			r.response.Body.Close()
		}
		r.dotter.Dot(dot.HTTPDotType, APINameGetFile, false)
		err = errors.New(r.response.Status)
		return
	}
	r.dotter.Dot(dot.HTTPDotType, APINameGetFile, true)
	return
}

func (r *urlReader) Close() (err error) {
	if r.response != nil {
		err = r.response.Body.Close()
		r.response = nil
	}
	r.closed = true
	return
}

func (d *Downloader) downloadBytesInner(ctx context.Context, host, key string) ([]byte, error) {
	if strings.HasPrefix(key, "/") {
		key = strings.TrimPrefix(key, "/")
	}

	url := fmt.Sprintf("%s/getfile/%s/%s/%s", host, d.credentials.AccessKey, d.bucket, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	response, err := d.downloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, errors.New(response.Status)
	}
	return ioutil.ReadAll(response.Body)
}

func generateRange(offset, size int64) string {
	if offset == -1 {
		return fmt.Sprintf("bytes=-%d", size)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+size)
}

func (d *Downloader) downloadRangeBytesInner(ctx context.Context, host, key string, offset, size int64) (int64, []byte, error) {
	if strings.HasPrefix(key, "/") {
		key = strings.TrimPrefix(key, "/")
	}

	url := fmt.Sprintf("%s/getfile/%s/%s/%s", host, d.credentials.AccessKey, d.bucket, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return -1, nil, err
	}

	req.Header.Set("Range", generateRange(offset, size))
	response, err := d.downloadClient.Do(req)
	if err != nil {
		return -1, nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusPartialContent {
		return -1, nil, errors.New(response.Status)
	}

	rangeResponse := response.Header.Get("Content-Range")
	if rangeResponse == "" {
		return -1, nil, errors.New("no content range")
	}

	l, err := getTotalLength(rangeResponse)
	if err != nil {
		return -1, nil, err
	}
	b, err := ioutil.ReadAll(response.Body)
	return l, b, err
}

func getTotalLength(crange string) (int64, error) {
	cr := strings.Split(crange, "/")
	if len(cr) != 2 {
		return -1, errors.New("wrong range " + crange)
	}

	return strconv.ParseInt(cr[1], 10, 64)
}
