// hsbench.go
// Copyright (c) 2017 Wasabi Technology, Inc.
// Copyright (c) 2019 Red Hat Inc.

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Global variables
var access_key, secret_key, url_host, bucket_prefix, object_prefix, region, modes, output, json_output, sizeArg string
var buckets []string
var duration_secs, threads, loops int
var object_data []byte
var object_data_md5 string
var max_keys, running_threads, bucket_count, object_count, object_size, op_counter int64
var object_count_flag bool
var endtime time.Time
var interval float64
var zero_object_data bool
var force_http1, randomize_suffix bool
var randomize_seed int64
var loop_objects bool

var listMu sync.Mutex
var listContinuationToken []*string
var listBucketComplete []bool

// canonicalAmzHeaders -- return the x-amz headers canonicalized
func canonicalAmzHeaders(req *http.Request) string {
	// Parse out all x-amz headers
	var headers []string
	for header := range req.Header {
		norm := strings.ToLower(strings.TrimSpace(header))
		if strings.HasPrefix(norm, "x-amz") {
			headers = append(headers, norm)
		}
	}
	// Put them in sorted order
	sort.Strings(headers)
	// Now add back the values
	for n, header := range headers {
		headers[n] = header + ":" + strings.Replace(req.Header.Get(header), "\n", " ", -1)
	}
	// Finally, put them back together
	if len(headers) > 0 {
		return strings.Join(headers, "\n") + "\n"
	} else {
		return ""
	}
}

func hmacSHA1(key []byte, content string) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(content))
	return mac.Sum(nil)
}

func setSignature(req *http.Request) {
	// Setup default parameters
	dateHdr := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", dateHdr)
	// Get the canonical resource and header
	canonicalResource := req.URL.EscapedPath()
	canonicalHeaders := canonicalAmzHeaders(req)
	stringToSign := req.Method + "\n" + req.Header.Get("Content-MD5") + "\n" + req.Header.Get("Content-Type") + "\n\n" +
		canonicalHeaders + canonicalResource
	hash := hmacSHA1([]byte(secret_key), stringToSign)
	signature := base64.StdEncoding.EncodeToString(hash)
	req.Header.Set("Authorization", fmt.Sprintf("AWS %s:%s", access_key, signature))
}

type IntervalStats struct {
	loop         int
	name         string
	mode         string
	bytes        int64
	slowdowns    int64
	intervalNano int64
	latNano      []int64
}

func (is *IntervalStats) makeOutputStats() OutputStats {
	// Compute and log the stats
	ops := len(is.latNano)
	totalLat := int64(0)
	minLat := float64(0)
	maxLat := float64(0)
	Lat99 := float64(0)
	Lat95 := float64(0)
	Lat90 := float64(0)
	Lat75 := float64(0)
	Lat50 := float64(0)
	avgLat := float64(0)
	if ops > 0 {
		minLat = float64(is.latNano[0]) / 1000000
		maxLat = float64(is.latNano[ops-1]) / 1000000
		for i := range is.latNano {
			totalLat += is.latNano[i]
		}
		avgLat = float64(totalLat) / float64(ops) / 1000000
		Lat99Nano := is.latNano[int64(math.Round(0.99*float64(ops)))-1]
		Lat99 = float64(Lat99Nano) / 1000000
		Lat95Nano := is.latNano[int64(math.Round(0.95*float64(ops)))-1]
		Lat95 = float64(Lat95Nano) / 1000000
		Lat90Nano := is.latNano[int64(math.Round(0.9*float64(ops)))-1]
		Lat90 = float64(Lat90Nano) / 1000000
		Lat75Nano := is.latNano[int64(math.Round(0.75*float64(ops)))-1]
		Lat75 = float64(Lat75Nano) / 1000000
		Lat50Nano := is.latNano[int64(math.Round(0.5*float64(ops)))-1]
		Lat50 = float64(Lat50Nano) / 1000000
	}
	seconds := float64(is.intervalNano) / 1000000000
	mbps := float64(is.bytes) / seconds / bytefmt.MEGABYTE
	iops := float64(ops) / seconds

	return OutputStats{
		is.loop,
		is.name,
		seconds,
		is.mode,
		ops,
		mbps,
		iops,
		minLat,
		avgLat,
		Lat99,
		Lat95,
		Lat90,
		Lat75,
		Lat50,
		maxLat,
		is.slowdowns}
}

type OutputStats struct {
	Loop         int
	IntervalName string
	Seconds      float64
	Mode         string
	Ops          int
	Mbps         float64
	Iops         float64
	MinLat       float64
	AvgLat       float64
	Lat99        float64
	Lat95        float64
	Lat90        float64
	Lat75        float64
	Lat50        float64
	MaxLat       float64
	Slowdowns    int64
}

func (o *OutputStats) log() {
	log.Printf(
		"Loop: %d, Int: %s, Dur(s): %.1f, Mode: %s, Ops: %d, MB/s: %.2f, IO/s: %.0f, Lat(ms): [ min: %.1f, avg: %.1f, 99%%: %.1f, 95%%: %.1f, 90%%: %.1f, 75%%: %.1f, 50%%: %.1f, max: %.1f ], Slowdowns: %d",
		o.Loop,
		o.IntervalName,
		o.Seconds,
		o.Mode,
		o.Ops,
		o.Mbps,
		o.Iops,
		o.MinLat,
		o.AvgLat,
		o.Lat99,
		o.Lat95,
		o.Lat90,
		o.Lat75,
		o.Lat50,
		o.MaxLat,
		o.Slowdowns)
}

func (o *OutputStats) csv_header(w *csv.Writer) {
	if w == nil {
		log.Fatal("OutputStats passed nil CSV writer")
	}

	s := []string{
		"Loop",
		"Inteval",
		"Duration(s)",
		"Mode", "Ops",
		"MB/s",
		"IO/s",
		"Min Latency (ms)",
		"Avg Latency(ms)",
		"99% Latency(ms)",
		"95% Latency(ms)",
		"90% Latency(ms)",
		"75% Latency(ms)",
		"50% Latency(ms)",
		"Max Latency(ms)",
		"Slowdowns"}

	if err := w.Write(s); err != nil {
		log.Fatal("Error writing to CSV writer: ", err)
	}
}

func (o *OutputStats) csv(w *csv.Writer) {
	if w == nil {
		log.Fatal("OutputStats Passed nil csv writer")
	}

	s := []string{
		strconv.Itoa(o.Loop),
		o.IntervalName,
		strconv.FormatFloat(o.Seconds, 'f', 2, 64),
		o.Mode,
		strconv.Itoa(o.Ops),
		strconv.FormatFloat(o.Mbps, 'f', 2, 64),
		strconv.FormatFloat(o.Iops, 'f', 2, 64),
		strconv.FormatFloat(o.MinLat, 'f', 2, 64),
		strconv.FormatFloat(o.AvgLat, 'f', 2, 64),
		strconv.FormatFloat(o.Lat99, 'f', 2, 64),
		strconv.FormatFloat(o.Lat95, 'f', 2, 64),
		strconv.FormatFloat(o.Lat90, 'f', 2, 64),
		strconv.FormatFloat(o.Lat75, 'f', 2, 64),
		strconv.FormatFloat(o.Lat50, 'f', 2, 64),
		strconv.FormatFloat(o.MaxLat, 'f', 2, 64),
		strconv.FormatInt(o.Slowdowns, 10)}

	if err := w.Write(s); err != nil {
		log.Fatal("Error writing to CSV writer: ", err)
	}
}

func (o *OutputStats) json(jfile *os.File) {
	if jfile == nil {
		log.Fatal("OutputStats passed nil JSON file")
	}
	jdata, err := json.Marshal(o)
	if err != nil {
		log.Fatal("Error marshaling JSON: ", err)
	}
	log.Println(string(jdata))
	_, err = jfile.WriteString(string(jdata) + "\n")
	if err != nil {
		log.Fatal("Error writing to JSON file: ", err)
	}
}

type ThreadStats struct {
	start       int64
	curInterval int64
	intervals   []IntervalStats
}

func makeThreadStats(s int64, loop int, mode string, intervalNano int64) ThreadStats {
	ts := ThreadStats{s, 0, []IntervalStats{}}
	ts.intervals = append(ts.intervals, IntervalStats{loop, "0", mode, 0, 0, intervalNano, []int64{}})
	return ts
}

func (ts *ThreadStats) updateIntervals(loop int, mode string, intervalNano int64) int64 {
	// Interval statistics disabled, so just return the current interval
	if intervalNano < 0 {
		return ts.curInterval
	}
	for ts.start+intervalNano*(ts.curInterval+1) < time.Now().UnixNano() {
		ts.curInterval++
		ts.intervals = append(
			ts.intervals,
			IntervalStats{
				loop,
				strconv.FormatInt(ts.curInterval, 10),
				mode,
				0,
				0,
				intervalNano,
				[]int64{}})
	}
	return ts.curInterval
}

func (ts *ThreadStats) finish() {
	ts.curInterval = -1
}

type Stats struct {
	// threads
	threads int
	// The loop we are in
	loop int
	// Test mode being run
	mode string
	// start time in nanoseconds
	startNano int64
	// end time in nanoseconds
	endNano int64
	// Duration in nanoseconds for each interval
	intervalNano int64
	// Per-thread statistics
	threadStats []ThreadStats
	// a map of per-interval thread completion counters
	intervalCompletions sync.Map
	// a counter of how many threads have finished updating stats entirely
	completions int32
}

func makeStats(loop int, mode string, threads int, intervalNano int64) Stats {
	start := time.Now().UnixNano()
	s := Stats{threads, loop, mode, start, 0, intervalNano, []ThreadStats{}, sync.Map{}, 0}
	for i := 0; i < threads; i++ {
		s.threadStats = append(s.threadStats, makeThreadStats(start, s.loop, s.mode, s.intervalNano))
		s.updateIntervals(i)
	}
	return s
}

func (stats *Stats) makeOutputStats(i int64) (OutputStats, bool) {
	// Check bounds first
	if stats.intervalNano < 0 || i < 0 {
		return OutputStats{}, false
	}
	// Not safe to log if not all writers have completed.
	value, ok := stats.intervalCompletions.Load(i)
	if !ok {
		return OutputStats{}, false
	}
	cp, ok := value.(*int32)
	if !ok {
		return OutputStats{}, false
	}
	count := atomic.LoadInt32(cp)
	if count < int32(stats.threads) {
		return OutputStats{}, false
	}

	bytes := int64(0)
	ops := int64(0)
	slowdowns := int64(0)

	for t := 0; t < stats.threads; t++ {
		bytes += stats.threadStats[t].intervals[i].bytes
		ops += int64(len(stats.threadStats[t].intervals[i].latNano))
		slowdowns += stats.threadStats[t].intervals[i].slowdowns
	}
	// Aggregate the per-thread Latency slice
	tmpLat := make([]int64, ops)
	var c int
	for t := 0; t < stats.threads; t++ {
		c += copy(tmpLat[c:], stats.threadStats[t].intervals[i].latNano)
	}
	sort.Slice(tmpLat, func(i, j int) bool { return tmpLat[i] < tmpLat[j] })
	is := IntervalStats{stats.loop, strconv.FormatInt(i, 10), stats.mode, bytes, slowdowns, stats.intervalNano, tmpLat}
	return is.makeOutputStats(), true
}

func (stats *Stats) makeTotalStats() (OutputStats, bool) {
	// Not safe to log if not all writers have completed.
	completions := atomic.LoadInt32(&stats.completions)
	if completions < int32(threads) {
		log.Printf("log, completions: %d", completions)
		return OutputStats{}, false
	}

	bytes := int64(0)
	ops := int64(0)
	slowdowns := int64(0)

	for t := 0; t < stats.threads; t++ {
		for i := 0; i < len(stats.threadStats[t].intervals); i++ {
			bytes += stats.threadStats[t].intervals[i].bytes
			ops += int64(len(stats.threadStats[t].intervals[i].latNano))
			slowdowns += stats.threadStats[t].intervals[i].slowdowns
		}
	}
	// Aggregate the per-thread Latency slice
	tmpLat := make([]int64, ops)
	var c int
	for t := 0; t < stats.threads; t++ {
		for i := 0; i < len(stats.threadStats[t].intervals); i++ {
			c += copy(tmpLat[c:], stats.threadStats[t].intervals[i].latNano)
		}
	}
	sort.Slice(tmpLat, func(i, j int) bool { return tmpLat[i] < tmpLat[j] })
	is := IntervalStats{stats.loop, "TOTAL", stats.mode, bytes, slowdowns, stats.endNano - stats.startNano, tmpLat}
	return is.makeOutputStats(), true
}

// Only safe to call from the calling thread
func (stats *Stats) updateIntervals(thread_num int) int64 {
	curInterval := stats.threadStats[thread_num].curInterval
	newInterval := stats.threadStats[thread_num].updateIntervals(stats.loop, stats.mode, stats.intervalNano)

	// Finish has already been called
	if curInterval < 0 {
		return -1
	}

	for i := curInterval; i < newInterval; i++ {
		// load or store the current value
		value, _ := stats.intervalCompletions.LoadOrStore(i, new(int32))
		cp, ok := value.(*int32)
		if !ok {
			log.Printf("updateIntervals: got data of type %T but wanted *int32", value)
			continue
		}

		count := atomic.AddInt32(cp, 1)
		if count == int32(stats.threads) {
			if is, ok := stats.makeOutputStats(i); ok {
				is.log()
			}
		}
	}
	return newInterval
}

func (stats *Stats) addOp(thread_num int, bytes int64, latNano int64) {

	// Interval statistics
	cur := stats.threadStats[thread_num].curInterval
	if cur < 0 {
		return
	}
	stats.threadStats[thread_num].intervals[cur].bytes += bytes
	stats.threadStats[thread_num].intervals[cur].latNano =
		append(stats.threadStats[thread_num].intervals[cur].latNano, latNano)
}

func (stats *Stats) addSlowDown(thread_num int) {
	cur := stats.threadStats[thread_num].curInterval
	stats.threadStats[thread_num].intervals[cur].slowdowns++
}

func (stats *Stats) finish(thread_num int) {
	stats.updateIntervals(thread_num)
	stats.threadStats[thread_num].finish()
	count := atomic.AddInt32(&stats.completions, 1)
	if count == int32(stats.threads) {
		stats.endNano = time.Now().UnixNano()
	}
}

func runUpload(thread_num int, fendtime time.Time, rand *ThreadSafeUUID, stats *Stats) {
	errcnt := 0
	svc := s3.New(session.New(), cfg)
	for {
		if duration_secs > -1 && time.Now().After(endtime) {
			break
		}
		objnum := atomic.AddInt64(&op_counter, 1)
		bucket_num := objnum % int64(bucket_count)
		if object_count > -1 && objnum >= object_count {
			objnum = atomic.AddInt64(&op_counter, -1)
			break
		}
		fileobj := bytes.NewReader(object_data)

		var key string
		if randomize_suffix {
			key = fmt.Sprintf("%s%s", object_prefix, rand.generateUUIDv4().String())
		} else {
			key = fmt.Sprintf("%s%012d", object_prefix, objnum)
		}
		r := &s3.PutObjectInput{
			Bucket: &buckets[bucket_num],
			Key:    &key,
			Body:   fileobj,
		}
		start := time.Now().UnixNano()
		req, _ := svc.PutObjectRequest(r)
		// Disable payload checksum calculation (very expensive)
		req.HTTPRequest.Header.Add("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		err := req.Send()
		end := time.Now().UnixNano()
		stats.updateIntervals(thread_num)

		if err != nil {
			errcnt++
			stats.addSlowDown(thread_num)
			atomic.AddInt64(&op_counter, -1)
			log.Printf("upload err", err)
		} else {
			// Update the stats
			stats.addOp(thread_num, object_size, end-start)
		}
		if errcnt > 2 {
			break
		}
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runDownload(thread_num int, fendtime time.Time, rand *ThreadSafeUUID, stats *Stats) {
	errcnt := 0
	svc := s3.New(session.New(), cfg)
	for {
		if duration_secs > -1 && time.Now().After(endtime) {
			break
		}

		objnum := atomic.AddInt64(&op_counter, 1)
		if loop_objects && duration_secs > -1 {
			objnum = objnum % object_count
		}
		if object_count > -1 && objnum >= object_count {
			atomic.AddInt64(&op_counter, -1)
			break
		}

		bucket_num := objnum % int64(bucket_count)
		var key string
		if randomize_suffix {
			key = fmt.Sprintf("%s%s", object_prefix, rand.generateUUIDv4().String())
		} else {
			key = fmt.Sprintf("%s%012d", object_prefix, objnum)
		}
		r := &s3.GetObjectInput{
			Bucket: &buckets[bucket_num],
			Key:    &key,
		}

		start := time.Now().UnixNano()
		req, resp := svc.GetObjectRequest(r)
		err := req.Send()
		end := time.Now().UnixNano()
		stats.updateIntervals(thread_num)

		if err != nil {
			errcnt++
			stats.addSlowDown(thread_num)
			log.Printf("download err", err)
		} else {
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
			// Update the stats
			stats.addOp(thread_num, object_size, end-start)
		}
		if errcnt > 2 {
			break
		}

	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runDelete(thread_num int, rand *ThreadSafeUUID, stats *Stats) {
	errcnt := 0
	svc := s3.New(session.New(), cfg)
	for {
		if duration_secs > -1 && time.Now().After(endtime) {
			break
		}

		objnum := atomic.AddInt64(&op_counter, 1)
		if object_count > -1 && objnum >= object_count {
			atomic.AddInt64(&op_counter, -1)
			break
		}

		bucket_num := objnum % int64(bucket_count)

		var key string
		if randomize_suffix {
			key = fmt.Sprintf("%s%s", object_prefix, rand.generateUUIDv4().String())
		} else {
			key = fmt.Sprintf("%s%012d", object_prefix, objnum)
		}
		r := &s3.DeleteObjectInput{
			Bucket: &buckets[bucket_num],
			Key:    &key,
		}

		start := time.Now().UnixNano()
		req, out := svc.DeleteObjectRequest(r)
		err := req.Send()
		end := time.Now().UnixNano()
		stats.updateIntervals(thread_num)

		if err != nil {
			errcnt++
			stats.addSlowDown(thread_num)
			log.Printf("delete err", err, "out", out.String())
		} else {
			// Update the stats
			stats.addOp(thread_num, object_size, end-start)
		}
		if errcnt > 2 {
			break
		}
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runBucketDelete(thread_num int, stats *Stats) {
	svc := s3.New(session.New(), cfg)

	for {
		bucket_num := atomic.AddInt64(&op_counter, 1)
		if bucket_num >= bucket_count {
			atomic.AddInt64(&op_counter, -1)
			break
		}
		r := &s3.DeleteBucketInput{
			Bucket: &buckets[bucket_num],
		}

		start := time.Now().UnixNano()
		_, err := svc.DeleteBucket(r)
		end := time.Now().UnixNano()
		stats.updateIntervals(thread_num)

		if err != nil {
			break
		}
		stats.addOp(thread_num, 0, end-start)
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runBucketList(thread_num int, stats *Stats) {
	svc := s3.New(session.New(), cfg)

	for {
		bucket_num := atomic.AddInt64(&op_counter, 1)
		if bucket_num >= bucket_count {
			atomic.AddInt64(&op_counter, -1)
			break
		}

		start := time.Now().UnixNano()
		err := svc.ListObjectsPages(
			&s3.ListObjectsInput{
				Bucket:  &buckets[bucket_num],
				MaxKeys: &max_keys,
			},
			func(p *s3.ListObjectsOutput, last bool) bool {
				end := time.Now().UnixNano()
				stats.updateIntervals(thread_num)
				stats.addOp(thread_num, 0, end-start)
				start = time.Now().UnixNano()
				return true
			})

		if err != nil {
			break
		}
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

var cfg *aws.Config

func runBucketsInit(thread_num int, stats *Stats) {
	svc := s3.New(session.New(), cfg)

	for {
		bucket_num := atomic.AddInt64(&op_counter, 1)
		if bucket_num >= bucket_count {
			atomic.AddInt64(&op_counter, -1)
			break
		}
		start := time.Now().UnixNano()
		in := &s3.CreateBucketInput{Bucket: aws.String(buckets[bucket_num])}
		_, err := svc.CreateBucket(in)
		end := time.Now().UnixNano()
		stats.updateIntervals(thread_num)

		if err != nil {
			if !strings.Contains(err.Error(), s3.ErrCodeBucketAlreadyOwnedByYou) &&
				!strings.Contains(err.Error(), "BucketAlreadyExists") {
				log.Fatalf("FATAL: Unable to create bucket %s (is your access and secret correct?): %v", buckets[bucket_num], err)
			}
		}
		stats.addOp(thread_num, 0, end-start)
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runBucketsClear(thread_num int, stats *Stats) {
	svc := s3.New(session.New(), cfg)

	for current_bucket := range bucket_count {
		bucket_num := (thread_num + int(current_bucket)) % int(bucket_count)
		log.Printf("Clearing bucket %s num %d thread num %d", buckets[bucket_num], bucket_num, thread_num)
		listMu.Lock()
		if listBucketComplete[bucket_num] {
			listMu.Unlock()
			log.Printf("abort reading bucket %s in thread %d since bucket is read", buckets[bucket_num], thread_num)
			break
		}
		out, err := svc.ListObjectsV2(&s3.ListObjectsV2Input{
			Bucket:            &buckets[bucket_num],
			ContinuationToken: listContinuationToken[bucket_num],
			MaxKeys:           &max_keys,
		})
		if err != nil {
			listMu.Unlock()
			break
		}
		if out.NextContinuationToken == nil {
			listBucketComplete[bucket_num] = true
			log.Printf("Reached end in bucket %s by thread %d", buckets[bucket_num], thread_num)
		}
		listContinuationToken[bucket_num] = out.NextContinuationToken
		listMu.Unlock()
		n := len(out.Contents)
		for n > 0 {
			log.Printf("Received %d objects from bucket %s in thread %d", n, buckets[bucket_num], thread_num)
			for _, v := range out.Contents {
				start := time.Now().UnixNano()
				svc.DeleteObject(&s3.DeleteObjectInput{
					Bucket: &buckets[bucket_num],
					Key:    v.Key,
				})
				end := time.Now().UnixNano()
				stats.updateIntervals(thread_num)
				stats.addOp(thread_num, *v.Size, end-start)
			}
			listMu.Lock()
			if listBucketComplete[bucket_num] {
				listMu.Unlock()
				n = 0
				continue
			}
			out, err = svc.ListObjectsV2(
				&s3.ListObjectsV2Input{
					Bucket:            &buckets[bucket_num],
					ContinuationToken: listContinuationToken[bucket_num],
					MaxKeys:           &max_keys,
				},
			)
			if err != nil {
				listMu.Unlock()
				break
			}
			if out.NextContinuationToken == nil {
				listBucketComplete[bucket_num] = true
				log.Printf("Reached end in bucket %s by thread %d", buckets[bucket_num], thread_num)
			}
			listContinuationToken[bucket_num] = out.NextContinuationToken
			listMu.Unlock()
			n = len(out.Contents)
		}
	}
	stats.finish(thread_num)
	atomic.AddInt64(&running_threads, -1)
}

func runWrapper(loop int, r rune) []OutputStats {
	op_counter = -1
	running_threads = int64(threads)
	intervalNano := int64(interval * 1000000000)
	endtime = time.Now().Add(time.Second * time.Duration(duration_secs))
	var stats Stats

	// If we perviously set the object count after running a put
	// test, set the object count back to -1 for the new put test.
	if r == 'p' && object_count_flag {
		object_count = -1
		object_count_flag = false
	}

	rnd := NewThreadSafeUUID(randomize_seed)

	switch r {
	case 'c':
		log.Printf("Running Loop %d BUCKET CLEAR TEST", loop)
		stats = makeStats(loop, "BCLR", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runBucketsClear(n, &stats)
		}
	case 'x':
		log.Printf("Running Loop %d BUCKET DELETE TEST", loop)
		stats = makeStats(loop, "BDEL", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runBucketDelete(n, &stats)
		}
	case 'i':
		log.Printf("Running Loop %d BUCKET INIT TEST", loop)
		stats = makeStats(loop, "BINIT", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runBucketsInit(n, &stats)
		}
	case 'p':
		log.Printf("Running Loop %d OBJECT PUT TEST", loop)
		stats = makeStats(loop, "PUT", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runUpload(n, endtime, rnd, &stats)
		}
	case 'l':
		log.Printf("Running Loop %d BUCKET LIST TEST", loop)
		stats = makeStats(loop, "LIST", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runBucketList(n, &stats)
		}
	case 'g':
		log.Printf("Running Loop %d OBJECT GET TEST", loop)
		stats = makeStats(loop, "GET", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runDownload(n, endtime, rnd, &stats)
		}
	case 'd':
		log.Printf("Running Loop %d OBJECT DELETE TEST", loop)
		stats = makeStats(loop, "DEL", threads, intervalNano)
		for n := 0; n < threads; n++ {
			go runDelete(n, rnd, &stats)
		}
	}

	// Wait for it to finish
	for atomic.LoadInt64(&running_threads) > 0 {
		time.Sleep(time.Millisecond)
	}

	// If the user didn't set the object_count, we can set it here
	// to limit subsequent get/del tests to valid objects only.
	if r == 'p' && object_count < 0 {
		object_count = op_counter + 1
		object_count_flag = true
	}

	// Create the Output Stats
	os := make([]OutputStats, 0)
	for i := int64(0); i >= 0; i++ {
		if o, ok := stats.makeOutputStats(i); ok {
			os = append(os, o)
		} else {
			break
		}
	}
	if o, ok := stats.makeTotalStats(); ok {
		o.log()
		os = append(os, o)
	}
	return os
}

func init() {
	// Parse command line
	myflag := flag.NewFlagSet("myflag", flag.ExitOnError)
	myflag.StringVar(&access_key, "a", os.Getenv("AWS_ACCESS_KEY_ID"), "Access key")
	myflag.StringVar(&secret_key, "s", os.Getenv("AWS_SECRET_ACCESS_KEY"), "Secret key")
	myflag.StringVar(&url_host, "u", os.Getenv("AWS_HOST"), "URL for host with method prefix")
	myflag.StringVar(&object_prefix, "op", "", "Prefix for objects")
	myflag.BoolVar(&force_http1, "fh", false, "Force HTTP1")
	myflag.BoolVar(&randomize_suffix, "rs", false, "Randomize object name suffix")
	myflag.BoolVar(&loop_objects, "lo", false, "Loop objects on get operation")
	myflag.Int64Var(&randomize_seed, "sd", 0, "Randomize object name suffix")
	myflag.StringVar(&bucket_prefix, "bp", "hotsauce-bench", "Prefix for buckets")
	myflag.StringVar(&region, "r", "us-east-1", "Region for testing")
	myflag.StringVar(&modes, "m", "cxiplgdcx", "Run modes in order.  See NOTES for more info")
	myflag.StringVar(&output, "o", "", "Write CSV output to this file")
	myflag.StringVar(&json_output, "j", "", "Write JSON output to this file")
	myflag.Int64Var(&max_keys, "mk", 1000, "Maximum number of keys to retreive at once for bucket listings")
	myflag.Int64Var(&object_count, "n", -1, "Maximum number of objects <-1 for unlimited>")
	myflag.Int64Var(&bucket_count, "b", 1, "Number of buckets to distribute IOs across")
	myflag.IntVar(&duration_secs, "d", 60, "Maximum test duration in seconds <-1 for unlimited>")
	myflag.IntVar(&threads, "t", 1, "Number of threads to run")
	myflag.IntVar(&loops, "l", 1, "Number of times to repeat test")
	myflag.StringVar(&sizeArg, "z", "1M", "Size of objects in bytes with postfix K, M, and G")
	myflag.Float64Var(&interval, "ri", 1.0, "Number of seconds between report intervals")
	myflag.BoolVar(&zero_object_data, "zd", false, "Write zero values for objects data in PUT operations instead of random data")
	// define custom usage output with notes
	notes :=
		`
NOTES:
  - Valid mode types for the -m mode string are:
    c: clear all existing objects from buckets (requires lookups)
    x: delete buckets
    i: initialize buckets 
    p: put objects in buckets
    l: list objects in buckets
    g: get objects from buckets
    d: delete objects from buckets 

    These modes are processed in-order and can be repeated, ie "ippgd" will
    initialize the buckets, put the objects, reput the objects, get the
    objects, and then delete the objects.  The repeat flag will repeat this
    whole process the specified number of times.

  - When performing bucket listings, many S3 storage systems limit the
    maximum number of keys returned to 1000 even if MaxKeys is set higher.
    hsbench will attempt to set MaxKeys to whatever value is passed via the 
    "mk" flag, but it's likely that any values above 1000 will be ignored.
`
	myflag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "\nUSAGE: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "OPTIONS:\n")
		myflag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), notes)
	}

	if err := myflag.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	// Check the arguments
	if object_count < 0 && duration_secs < 0 {
		log.Fatal("The number of objects and duration can not both be unlimited")
	}
	if access_key == "" {
		log.Fatal("Missing argument -a for access key.")
	}
	if secret_key == "" {
		log.Fatal("Missing argument -s for secret key.")
	}
	if url_host == "" {
		log.Fatal("Missing argument -u for host endpoint.")
	}
	invalid_mode := false
	for _, r := range modes {
		if r != 'i' &&
			r != 'c' &&
			r != 'p' &&
			r != 'g' &&
			r != 'l' &&
			r != 'd' &&
			r != 'x' {
			s := fmt.Sprintf("Invalid mode '%s' passed to -m", string(r))
			log.Printf(s)
			invalid_mode = true
		}
	}
	if invalid_mode {
		log.Fatal("Invalid modes passed to -m, see help for details.")
	}
	var err error
	var size uint64
	if size, err = bytefmt.ToBytes(sizeArg); err != nil {
		log.Fatalf("Invalid -z argument for object size: %v", err)
	}
	object_size = int64(size)
	listContinuationToken = make([]*string, bucket_count)
	listBucketComplete = make([]bool, bucket_count)
	log.Printf("list %v", listContinuationToken)
}

func initData() {
	// Initialize data for the bucket
	object_data = make([]byte, object_size)
	if zero_object_data {
		for i := range object_data {
			object_data[i] = 0
		}
	} else {
		rand.Read(object_data)
	}
	hasher := md5.New()
	hasher.Write(object_data)
	object_data_md5 = base64.StdEncoding.EncodeToString(hasher.Sum(nil))
}

func main() {
	// Hello
	log.Printf("Hotsauce S3 Benchmark Version 0.1")

	cfg = &aws.Config{
		Endpoint:    aws.String(url_host),
		Credentials: credentials.NewStaticCredentials(access_key, secret_key, ""),
		Region:      aws.String(region),
		// DisableParamValidation:  aws.Bool(true),
		DisableComputeChecksums: aws.Bool(true),
		S3ForcePathStyle:        aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				ForceAttemptHTTP2: force_http1,
			},
		},
	}

	// Echo the parameters
	log.Printf("Parameters:")
	log.Printf("url=%s", url_host)
	log.Printf("object_prefix=%s", object_prefix)
	log.Printf("bucket_prefix=%s", bucket_prefix)
	log.Printf("region=%s", region)
	log.Printf("modes=%s", modes)
	log.Printf("output=%s", output)
	log.Printf("json_output=%s", json_output)
	log.Printf("max_keys=%d", max_keys)
	log.Printf("object_count=%d", object_count)
	log.Printf("bucket_count=%d", bucket_count)
	log.Printf("duration=%d", duration_secs)
	log.Printf("threads=%d", threads)
	log.Printf("loops=%d", loops)
	log.Printf("size=%s", sizeArg)
	log.Printf("interval=%f", interval)
	log.Printf("force_http1=%t", force_http1)
	log.Printf("randomize_suffix=%t", randomize_suffix)
	log.Printf("randomize_seed=%d", randomize_seed)

	// Init Data
	initData()

	// Setup the slice of buckets
	for i := int64(0); i < bucket_count; i++ {
		buckets = append(buckets, fmt.Sprintf("%s%012d", bucket_prefix, i))
	}

	// Loop running the tests
	oStats := make([]OutputStats, 0)
	for loop := 0; loop < loops; loop++ {
		for _, r := range modes {
			oStats = append(oStats, runWrapper(loop, r)...)
		}
	}

	// Write CSV Output
	if output != "" {
		file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY, 0777)
		defer file.Close()
		if err != nil {
			log.Fatal("Could not open CSV file for writing.")
		} else {
			csvWriter := csv.NewWriter(file)
			for i, o := range oStats {
				if i == 0 {
					o.csv_header(csvWriter)
				}
				o.csv(csvWriter)
			}
			csvWriter.Flush()
		}
	}

	// Write JSON output
	if json_output != "" {
		file, err := os.OpenFile(json_output, os.O_CREATE|os.O_WRONLY, 0777)
		defer file.Close()
		if err != nil {
			log.Fatal("Could not open JSON file for writing.")
		}
		data, err := json.Marshal(oStats)
		if err != nil {
			log.Fatal("Error marshaling JSON: ", err)
		}
		_, err = file.Write(data)
		if err != nil {
			log.Fatal("Error writing to JSON file: ", err)
		}
		file.Sync()
	}
}
