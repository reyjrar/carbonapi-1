package main

import (
	"bytes"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dgryski/carbonapi/expr"
	pb "github.com/dgryski/carbonzipper/carbonzipperpb"
	"github.com/dgryski/carbonzipper/mlog"
	"github.com/dgryski/carbonzipper/mstats"

	"github.com/bradfitz/gomemcache/memcache"
	ecache "github.com/dgryski/go-expirecache"
	"github.com/gorilla/handlers"
	"github.com/peterbourgon/g2g"
)

// Metrics contains exported counters and values for graphite
var Metrics = struct {
	Requests         *expvar.Int
	RequestCacheHits *expvar.Int

	FindRequests  *expvar.Int
	FindCacheHits *expvar.Int

	RenderRequests *expvar.Int

	MemcacheTimeouts *expvar.Int

	CacheSize  expvar.Func
	CacheItems expvar.Func
}{
	Requests:         expvar.NewInt("requests"),
	RequestCacheHits: expvar.NewInt("request_cache_hits"),

	FindRequests:  expvar.NewInt("find_requests"),
	FindCacheHits: expvar.NewInt("find_cache_hits"),

	RenderRequests: expvar.NewInt("render_requests"),

	MemcacheTimeouts: expvar.NewInt("memcache_timeouts"),
}

// BuildVersion is provided to be overridden at build time. Eg. go build -ldflags -X 'main.BuildVersion=...'
var BuildVersion = "(development build)"

var queryCache bytesCache
var findCache bytesCache

var timeFormats = []string{"15:04 20060102", "20060102", "01/02/06"}

var defaultTimeZone = time.Local

var logger mlog.Level

// Zipper is API entry to carbonzipper
var Zipper zipper

// Limiter limits concurrent zipper requests
var Limiter limiter

// for testing
var timeNow = time.Now

func writeResponse(w http.ResponseWriter, b []byte, format string, jsonp string) {

	switch format {
	case "json":
		if jsonp != "" {
			w.Header().Set("Content-Type", contentTypeJavaScript)
			w.Write([]byte(jsonp))
			w.Write([]byte{'('})
			w.Write(b)
			w.Write([]byte{')'})
		} else {
			w.Header().Set("Content-Type", contentTypeJSON)
			w.Write(b)
		}
	case "protobuf":
		w.Header().Set("Content-Type", contentTypeProtobuf)
		w.Write(b)
	case "raw":
		w.Header().Set("Content-Type", contentTypeRaw)
		w.Write(b)
	case "pickle":
		w.Header().Set("Content-Type", contentTypePickle)
		w.Write(b)
	case "csv":
		w.Header().Set("Content-Type", contentTypeCSV)
		w.Write(b)
	case "png":
		w.Header().Set("Content-Type", contentTypePNG)
		w.Write(b)
	}
}

const (
	contentTypeJSON       = "application/json"
	contentTypeProtobuf   = "application/x-protobuf"
	contentTypeJavaScript = "text/javascript"
	contentTypeRaw        = "text/plain"
	contentTypePickle     = "application/pickle"
	contentTypePNG        = "image/png"
	contentTypeCSV        = "text/csv"
)

type renderStats struct {
	zipperRequests int
}

func buildParseErrorString(target, e string, err error) string {
	msg := fmt.Sprintf("%s\n\n%-20s: %s\n", http.StatusText(http.StatusBadRequest), "Target", target)
	if err != nil {
		msg += fmt.Sprintf("%-20s: %s\n", "Error", err.Error())
	}
	if e != "" {
		msg += fmt.Sprintf("%-20s: %s\n%-20s: %s\n",
			"Parsed so far", target[0:len(target)-len(e)],
			"Could not parse", e)
	}
	return msg
}

// dateParamToEpoch turns a passed string parameter into a unix epoch
func dateParamToEpoch(s string, d int64) int32 {

	if s == "" {
		// return the default if nothing was passed
		return int32(d)
	}

	// relative timestamp
	if s[0] == '-' {
		offset, err := expr.IntervalString(s, -1)
		if err != nil {
			return int32(d)
		}

		return int32(timeNow().Add(time.Duration(offset) * time.Second).Unix())
	}

	if s == "now" {
		return int32(timeNow().Unix())
	}

	sint, err := strconv.Atoi(s)
	if err == nil && len(s) > 8 {
		return int32(sint) // We got a timestamp so returning it
	}

	if strings.Contains(s, "_") {
		s = strings.Replace(s, "_", " ", 1) // Go can't parse _ in date strings
	}

	for _, format := range timeFormats {
		t, err := time.ParseInLocation(format, s, defaultTimeZone)
		if err == nil {
			return int32(t.Unix())
		}
	}
	return int32(d)
}

func renderHandler(w http.ResponseWriter, r *http.Request, stats *renderStats) {

	Metrics.Requests.Add(1)

	err := r.ParseForm()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest)+": "+err.Error(), http.StatusBadRequest)
		return
	}

	targets := r.Form["target"]
	from := r.FormValue("from")
	until := r.FormValue("until")
	format := r.FormValue("format")
	useCache := !expr.TruthyBool(r.FormValue("noCache"))

	var jsonp string

	if format == "json" {
		// TODO(dgryski): check jsonp only has valid characters
		jsonp = r.FormValue("jsonp")
	}

	if format == "" && (expr.TruthyBool(r.FormValue("rawData")) || expr.TruthyBool(r.FormValue("rawdata"))) {
		format = "raw"
	}

	if format == "" {
		format = "png"
	}

	cacheTimeout := int32(60)

	if tstr := r.FormValue("cacheTimeout"); tstr != "" {
		t, err := strconv.Atoi(tstr)
		if err != nil {
			logger.Logf("failed to parse cacheTimeout: %v: %v", tstr, err)
		} else {
			cacheTimeout = int32(t)
		}
	}

	// make sure the cache key doesn't say noCache, because it will never hit
	r.Form.Del("noCache")

	// jsonp callback names are frequently autogenerated and hurt our cache
	r.Form.Del("jsonp")

	// Strip some cache-busters.  If you don't want to cache, use noCache=1
	r.Form.Del("_salt")
	r.Form.Del("_ts")
	r.Form.Del("_t") // Used by jquery.graphite.js

	cacheKey := r.Form.Encode()

	if response, ok := queryCache.get(cacheKey); useCache && ok {
		Metrics.RequestCacheHits.Add(1)
		writeResponse(w, response, format, jsonp)
		return
	}

	// normalize from and until values
	// BUG(dgryski): doesn't handle timezones the same as graphite-web
	from32 := dateParamToEpoch(from, timeNow().Add(-24*time.Hour).Unix())
	until32 := dateParamToEpoch(until, timeNow().Unix())
	if from32 == until32 {
		http.Error(w, "Invalid empty time range", http.StatusBadRequest)
		return
	}

	var results []*expr.MetricData
	var errors []string
	metricMap := make(map[expr.MetricRequest][]*expr.MetricData)

	for _, target := range targets {

		exp, e, err := expr.ParseExpr(target)

		if err != nil || e != "" {
			msg := buildParseErrorString(target, e, err)
			http.Error(w, msg, http.StatusBadRequest)
			return
		}

		for _, m := range exp.Metrics() {

			mfetch := m
			mfetch.From += from32
			mfetch.Until += until32

			if _, ok := metricMap[mfetch]; ok {
				// already fetched this metric for this request
				continue
			}

			var glob pb.GlobResponse
			var haveCacheData bool

			if response, ok := findCache.get(m.Metric); useCache && ok {
				Metrics.FindCacheHits.Add(1)
				err := glob.Unmarshal(response)
				haveCacheData = err == nil
			}

			if !haveCacheData {
				var err error
				Metrics.FindRequests.Add(1)
				stats.zipperRequests++
				glob, err = Zipper.Find(m.Metric)
				if err != nil {
					logger.Logf("Find: %v: %v", m.Metric, err)
					continue
				}
				b, err := glob.Marshal()
				if err == nil {
					findCache.set(m.Metric, b, 5*60)
				}
			}

			// For each metric returned in the Find response, query Render
			// This is a conscious decision to *not* cache render data
			rch := make(chan *expr.MetricData, len(glob.GetMatches()))
			leaves := 0
			for _, m := range glob.GetMatches() {
				if !m.GetIsLeaf() {
					continue
				}
				Metrics.RenderRequests.Add(1)
				leaves++
				Limiter.enter()
				stats.zipperRequests++
				go func(m *pb.GlobMatch, from, until int32) {
					var rptr *expr.MetricData
					r, err := Zipper.Render(m.GetPath(), from, until)
					if err == nil {
						rptr = &r
					} else {
						logger.Logf("Render: %v: %v", m.GetPath(), err)
					}
					rch <- rptr
					Limiter.leave()
				}(m, mfetch.From, mfetch.Until)
			}

			for i := 0; i < leaves; i++ {
				r := <-rch
				if r != nil {
					metricMap[mfetch] = append(metricMap[mfetch], r)
				}
			}

			expr.SortMetrics(metricMap[mfetch], mfetch)

		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					var buf [1024]byte
					runtime.Stack(buf[:], false)
					logger.Logf("panic during eval: %s: %s\n%s\n", cacheKey, r, string(buf[:]))
				}
			}()
			exprs, err := expr.EvalExpr(exp, from32, until32, metricMap)
			if err != nil && err != expr.ErrSeriesDoesNotExist {
				errors = append(errors, target+": "+err.Error())
				return
			}
			results = append(results, exprs...)
		}()
	}

	if len(errors) > 0 {
		errors = append([]string{"Encountered the following errors:"}, errors...)
		http.Error(w, strings.Join(errors, "\n"), http.StatusBadRequest)
		return
	}

	var body []byte

	switch format {
	case "json":
		if maxDataPoints, _ := strconv.Atoi(r.FormValue("maxDataPoints")); maxDataPoints != 0 {
			expr.ConsolidateJSON(maxDataPoints, results)
		}

		body = expr.MarshalJSON(results)
	case "protobuf":
		body, err = expr.MarshalProtobuf(results)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "raw":
		body = expr.MarshalRaw(results)
	case "csv":
		body = expr.MarshalCSV(results)
	case "pickle":
		body = expr.MarshalPickle(results)
	case "png":
		body = expr.MarshalPNG(r, results)
	}

	writeResponse(w, body, format, jsonp)

	if len(results) != 0 {
		queryCache.set(cacheKey, body, cacheTimeout)
	}
}

func findHandler(w http.ResponseWriter, r *http.Request) {

	format := r.FormValue("format")
	jsonp := r.FormValue("jsonp")

	query := r.FormValue("query")

	if query == "" {
		http.Error(w, "missing parameter `query`", http.StatusBadRequest)
		return
	}

	if format == "" {
		format = "treejson"
	}

	globs, err := Zipper.Find(query)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	var b []byte
	switch format {
	case "treejson", "json":
		b, err = findTreejson(globs)
		format = "json"
	case "completer":
		b, err = findCompleter(globs)
		format = "json"
	case "raw":
		b, err = findList(globs)
		format = "raw"
	}

	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	writeResponse(w, b, format, jsonp)
}

type completer struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	IsLeaf string `json:"is_leaf"`
}

func findCompleter(globs pb.GlobResponse) ([]byte, error) {
	var b bytes.Buffer

	var complete = make([]completer, 0)

	for _, g := range globs.GetMatches() {
		c := completer{
			Path: g.GetPath(),
		}

		if g.GetIsLeaf() {
			c.IsLeaf = "1"
		} else {
			c.IsLeaf = "0"
		}

		i := strings.LastIndex(c.Path, ".")

		if i != -1 {
			c.Name = c.Path[i+1:]
		}

		complete = append(complete, c)
	}

	err := json.NewEncoder(&b).Encode(struct {
		Metrics []completer `json:"metrics"`
	}{
		Metrics: complete},
	)
	return b.Bytes(), err
}

func findList(globs pb.GlobResponse) ([]byte, error) {
	var b bytes.Buffer

	for _, g := range globs.GetMatches() {

		var dot string
		// make sure non-leaves end in one dot
		if !g.GetIsLeaf() && !strings.HasSuffix(g.GetPath(), ".") {
			dot = "."
		}

		fmt.Fprintln(&b, g.GetPath()+dot)
	}

	return b.Bytes(), nil
}

type treejson struct {
	AllowChildren int            `json:"allowChildren"`
	Expandable    int            `json:"expandable"`
	Leaf          int            `json:"leaf"`
	ID            string         `json:"id"`
	Text          string         `json:"text"`
	Context       map[string]int `json:"context"` // unused
}

var treejsonContext = make(map[string]int)

func findTreejson(globs pb.GlobResponse) ([]byte, error) {
	var b bytes.Buffer

	var tree = make([]treejson, 0)

	seen := make(map[string]struct{})

	basepath := globs.GetName()

	if i := strings.LastIndex(basepath, "."); i != -1 {
		basepath = basepath[:i+1]
	}

	for _, g := range globs.GetMatches() {

		name := g.GetPath()

		if i := strings.LastIndex(name, "."); i != -1 {
			name = name[i+1:]
		}

		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		t := treejson{
			ID:      basepath + name,
			Context: treejsonContext,
			Text:    name,
		}

		if g.GetIsLeaf() {
			t.Leaf = 1
		} else {
			t.AllowChildren = 1
			t.Expandable = 1
		}

		tree = append(tree, t)
	}

	err := json.NewEncoder(&b).Encode(tree)
	return b.Bytes(), err
}

func passthroughHandler(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error

	if data, err = Zipper.Passthrough(r.URL.RequestURI()); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	w.Write(data)
}

func lbcheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Ok\n"))
}

var usageMsg = []byte(`
supported requests:
	/render/?target=
	/metrics/find/?query=
	/info/?target=
`)

func usageHandler(w http.ResponseWriter, r *http.Request) {
	w.Write(usageMsg)
}

func main() {

	z := flag.String("z", "", "zipper")
	port := flag.Int("p", 8080, "port")
	l := flag.Int("l", 20, "concurrency limit")
	cacheType := flag.String("cache", "mem", "cache type to use")
	mc := flag.String("mc", "", "comma separated memcached server list")
	memsize := flag.Int("memsize", 0, "in-memory cache size in MB (0 is unlimited)")
	cpus := flag.Int("cpus", 0, "number of CPUs to use")
	tz := flag.String("tz", "", "timezone,offset to use for dates with no timezone")
	graphiteHost := flag.String("graphite", "", "graphite destination host")
	logdir := flag.String("logdir", "/var/log/carbonapi/", "logging directory")
	logtostdout := flag.Bool("stdout", false, "log also to stdout")
	interval := flag.Duration("i", 60*time.Second, "interval to report internal statistics to graphite")
	idleconns := flag.Int("idleconns", 10, "max idle connections")

	flag.Parse()

	if *logdir == "" {
		mlog.SetRawStream(os.Stdout)
	} else {
		mlog.SetOutput(*logdir, "carbonapi", *logtostdout)
	}

	expvar.NewString("BuildVersion").Set(BuildVersion)
	logger.Logln("starting carbonapi", BuildVersion)

	if p := os.Getenv("PORT"); p != "" {
		*port, _ = strconv.Atoi(p)
	}

	Limiter = newLimiter(*l)

	if *z == "" {
		logger.Fatalln("no zipper provided")
	}

	if _, err := url.Parse(*z); err != nil {
		logger.Fatalln("unable to parze zipper:", err)
	}

	logger.Logln("using zipper", *z)
	Zipper = zipper{
		z: *z,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: *idleconns,
			},
		},
	}

	switch *cacheType {
	case "memcache":
		if *mc == "" {
			logger.Fatalln("memcache cache requested but no memcache servers provided")
		}

		servers := strings.Split(*mc, ",")
		logger.Logln("using memcache servers:", servers)
		queryCache = &memcachedCache{client: memcache.New(servers...)}
		findCache = &memcachedCache{client: memcache.New(servers...)}

	case "mem":
		qcache := &expireCache{ec: ecache.New(uint64(*memsize * 1024 * 1024))}
		queryCache = qcache
		go queryCache.(*expireCache).ec.ApproximateCleaner(10 * time.Second)

		findCache = &expireCache{ec: ecache.New(0)}
		go findCache.(*expireCache).ec.ApproximateCleaner(10 * time.Second)

		Metrics.CacheSize = expvar.Func(func() interface{} {
			return qcache.ec.Size()
		})
		expvar.Publish("cache_size", Metrics.CacheSize)

		Metrics.CacheItems = expvar.Func(func() interface{} {
			return qcache.ec.Items()
		})
		expvar.Publish("cache_items", Metrics.CacheItems)

	case "null":
		queryCache = &nullCache{}
		findCache = &nullCache{}
	}

	if *tz != "" {
		fields := strings.Split(*tz, ",")
		if len(fields) != 2 {
			logger.Fatalf("expected two fields for tz,seconds, got %d", len(fields))
		}

		var err error
		offs, err := strconv.Atoi(fields[1])
		if err != nil {
			logger.Fatalf("unable to parse seconds: %s: %s", fields[1], err)
		}

		defaultTimeZone = time.FixedZone(fields[0], offs)
		logger.Logf("using fixed timezone %s, offset %d ", defaultTimeZone.String(), offs)
	}

	if *cpus != 0 {
		logger.Logln("using GOMAXPROCS", *cpus)
		runtime.GOMAXPROCS(*cpus)
	}

	if envhost := os.Getenv("GRAPHITEHOST") + ":" + os.Getenv("GRAPHITEPORT"); envhost != ":" || *graphiteHost != "" {

		var host string

		switch {
		case envhost != ":" && *graphiteHost != "":
			host = *graphiteHost
		case envhost != ":":
			host = envhost
		case *graphiteHost != "":
			host = *graphiteHost
		}

		logger.Logln("Using graphite host", host)

		logger.Logln("setting stats interval to", *interval)

		// register our metrics with graphite
		graphite := g2g.NewGraphite(host, *interval, 10*time.Second)

		hostname, _ := os.Hostname()
		hostname = strings.Replace(hostname, ".", "_", -1)

		graphite.Register(fmt.Sprintf("carbon.api.%s.requests", hostname), Metrics.Requests)
		graphite.Register(fmt.Sprintf("carbon.api.%s.request_cache_hits", hostname), Metrics.RequestCacheHits)

		graphite.Register(fmt.Sprintf("carbon.api.%s.find_requests", hostname), Metrics.FindRequests)
		graphite.Register(fmt.Sprintf("carbon.api.%s.find_cache_hits", hostname), Metrics.FindCacheHits)

		graphite.Register(fmt.Sprintf("carbon.api.%s.render_requests", hostname), Metrics.RenderRequests)

		graphite.Register(fmt.Sprintf("carbon.api.%s.memcache_timeouts", hostname), Metrics.MemcacheTimeouts)

		if Metrics.CacheSize != nil {
			graphite.Register(fmt.Sprintf("carbon.api.%s.cache_size", hostname), Metrics.CacheSize)
			graphite.Register(fmt.Sprintf("carbon.api.%s.cache_items", hostname), Metrics.CacheItems)
		}

		go mstats.Start(*interval)

		graphite.Register(fmt.Sprintf("carbon.api.%s.alloc", hostname), &mstats.Alloc)
		graphite.Register(fmt.Sprintf("carbon.api.%s.total_alloc", hostname), &mstats.TotalAlloc)
		graphite.Register(fmt.Sprintf("carbon.api.%s.num_gc", hostname), &mstats.NumGC)
		graphite.Register(fmt.Sprintf("carbon.api.%s.pause_ns", hostname), &mstats.PauseNS)

	}

	render := func(w http.ResponseWriter, r *http.Request) {
		var stats renderStats
		t0 := time.Now()
		renderHandler(w, r, &stats)
		since := time.Since(t0)
		logger.Logln(r.RequestURI, since.Nanoseconds()/int64(time.Millisecond), stats.zipperRequests)
	}

	r := http.NewServeMux()
	r.HandleFunc("/render/", render)
	r.HandleFunc("/render", render)

	r.HandleFunc("/metrics/find/", findHandler)
	r.HandleFunc("/metrics/find", findHandler)

	r.HandleFunc("/info/", passthroughHandler)
	r.HandleFunc("/info", passthroughHandler)

	r.HandleFunc("/lb_check", lbcheckHandler)
	r.HandleFunc("/", usageHandler)

	logger.Logln("listening on port", *port)
	handler := handlers.CompressHandler(r)
	handler = handlers.CORS()(handler)
	handler = handlers.CombinedLoggingHandler(mlog.GetOutput(), handler)
	logger.Fatalln(http.ListenAndServe(":"+strconv.Itoa(*port), handler))
}
