package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// git克隆
// git clone -b 'v2.3.21' --single-branch --depth 1 <url>

// 使用go module
// export GOPROXY=https://goproxy.io   // 设置module代理
// go mod init m        // 初始化module或者从已有项目迁移(生成go.mod)
// go mod tidy          // 更新依赖
// go mod vendor        // 将所有依赖库复制到本地vendor目录
// go run -m=vendor main.go
// go build -mod=vendor // 利用本地vendor中的库构建或运行
// go list -u -m all    // 列出所有依赖库
// go mod edit -fmt     // 格式化go.mod

// 交叉编译:
// CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o gofs.exe main.go  // windows
// CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags netgo -o gofs main.go    // linux

// 解决alpine镜像问题, udp问题, 时区问题
// RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2 && apk add -U util-linux && apk add -U tzdata && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime  # 解决go语言程序无法在alpine执行的问题和syslog不支持udp的问题和时区问题

const maxUploadSize = 32 * (2 << 30) // 32 * 1GB
var dir, host, port string
var reqSeconds map[string]float64
var reqTimes map[string]int64

const html = `
<!DOCTYPE html>
<html lang="en">

<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <meta http-equiv="X-UA-Compatible" content="ie=edge" />
  <title>File Share</title>
  <!-- <script src="./bfi.js"></script> -->
</head>

<body>
  <p><strong>CMD Method</strong></p>
  <p>curl -X POST -F "path=bar" -F "file=@/root/foo/sample.pdf" {{.Protocol}}://{{.Host}}:{{.Port}}/upload</p>
  <p>curl -X GET {{.Protocol}}://{{.Host}}:{{.Port}}/bar/sample.pdf</p>
  <p>curl -X POST -d "filepath=bar/sample.pdf" {{.Protocol}}://{{.Host}}:{{.Port}}/delete</p>
  <p><strong>WEB Method</strong></p>
  <form enctype="multipart/form-data" action="{{.Protocol}}://{{.Host}}:{{.Port}}/upload" method="post" target="iiframe">
    <input name="path" placeholder="(Optional) remote storage path" size="30" />
    <input type="file" name="file" size="30" />
    <input type="submit" value="Upload" />
    <label> ¦ </label>
    <a href="{{.Protocol}}://{{.Host}}:{{.Port}}"><button type="button">Browse</button></a>
  </form>
  <iframe id="iiframe" name="iiframe" frameborder="0" width="600px" height="50px" ></iframe>
  <!-- <iframe id="iiframe" name="iiframe" frameborder="0" style="display:none;"></iframe> -->
</body>

</html>
`

func init() {
	reqSeconds = make(map[string]float64)
	reqTimes = make(map[string]int64)

	rand.Seed(time.Now().UnixNano())
}

type Server struct {
	Protocol string
	Host     string
	Port     string
}

// loggingMiddleware 是我们的日志中间件
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		var bodyBytes []byte
		var bodyString string

		// 只对 POST, PUT, PATCH 方法读取请求体
		// 因为 GET, DELETE 等方法通常没有请求体
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			var err error
			// 读取 r.Body 内容
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				log.Printf("错误：无法读取请求体: %v", err)
			} else {
				// 将读出的内容写回 r.Body，以便后续的 handler 可以继续使用
				r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}

		// 调用链中的下一个处理器
		next.ServeHTTP(lrw, r)

		duration := time.Since(startTime)

		// 如果有请求体，将其转换为字符串
		if len(bodyBytes) > 0 {
			bodyString = string(bodyBytes)
		} else {
			bodyString = "[无请求体]"
		}

		log.Printf(
			"来源: %s | 方法: %s | 路径: %s | 状态码: %d | 耗时: %v | 请求头: %s",
			r.RemoteAddr,
			r.Method,
			r.URL.Path,
			lrw.statusCode,
			duration,
			bodyString,
		)
	})
}

// loggingResponseWriter 包装了 http.ResponseWriter，用于捕获状态码
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Gzip Compression
type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func Gzip(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func(t time.Time) {
			reqTimes[r.URL.Path]++
			reqSeconds[r.URL.Path] += timeCost(t)
		}(time.Now())

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		gzw := gzipResponseWriter{Writer: gz, ResponseWriter: w}
		handler.ServeHTTP(gzw, r)
	})
}

func GetLocalIP() string {
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addrs {
			// check the address type and if it is not a loopback the display it
			if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

func timeCost(start time.Time) float64 {
	return time.Since(start).Seconds()
}

// delete file
// curl -X POST -d "filepath=bar/sample.pdf" http://127.0.0.1:2333/delete
func delete(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	if r.Method == "POST" {
		r.ParseForm()
		fpath := strings.TrimSpace(r.FormValue("filepath"))
		if fpath == "" {
			log.Println("Delete file error: no file specified")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "✘ Failed: no file specified")
			return
		}

		// fmt.Println(dir, fpath, handler.Filename)
		fullpath := filepath.Join(dir, fpath)

		if err := os.RemoveAll(fullpath); err != nil {
			log.Println("Delete file error: ", err.Error())
			fmt.Fprintf(w, "✘ Failed: %s", err.Error())
			return
		}

		log.Println("Delete file", fpath, "successfully")
		fmt.Fprintf(w, "✔ Succeeded")
	} else {
		log.Println("Delete file error: requst method must be post")
		fmt.Fprintf(w, "✘ Failed: requst method must be post")
	}
}

// upload file
// curl -X POST -F "path=test" -F "file=@/home/xshrim/a.js" http://127.0.0.1:2333/upload
// curl -X POST -F "file=@/home/xshrim/a.js" http://127.0.0.1:2333/upload/test/a.js
func upload(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	pl := "http"
	ht := host
	pt := port

	if wh := os.Getenv("WEBHOST"); wh != "" {
		ht = wh
		pt = "80"
		if wp := os.Getenv("WEBPORT"); wp != "" {
			pt = wp
		}
	}
	if wl := os.Getenv("WEBPROTOCOL"); wl != "" {
		pl = strings.ToLower(wl)
		if pl == "https" {
			pt = "443"
		}
		if wp := os.Getenv("WEBPORT"); wp != "" {
			pt = wp
		}
	}

	if r.Method == "GET" {
		// crutime := time.Now().Unix()
		// h := md5.New()
		// io.WriteString(h, strconv.FormatInt(crutime, 10))
		// token := fmt.Sprintf("%x", h.Sum(nil))
		// t, _ := template.ParseFiles("front.html")

		t, _ := template.New("index").Parse(html)

		// t.Execute(w, token)
		t.Execute(w, &Server{
			Protocol: pl,
			Host:     ht,
			Port:     pt,
		})
		return
	}

	r.ParseMultipartForm(maxUploadSize)

	fpath := strings.TrimSpace(r.FormValue("path"))

	file, handler, err := r.FormFile("file")
	if err != nil {
		log.Println("Receive file error: ", err.Error())
		// w.WriteHeader(http.StatusNoContent)
		fmt.Fprintf(w, "✘ Failed: "+err.Error())
		return
	}
	defer file.Close()

	log.Println(fmt.Sprintf("Receiving file [filename: %+v, filesize: %+vB, httpheader: %+v", handler.Filename, handler.Size, handler.Header))

	fileBytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Println("Receive file error: ", err.Error())
		w.WriteHeader(http.StatusNoContent)
		fmt.Fprintf(w, "✘ Failed: "+err.Error())
		return
	}

	// tempFile, err := ioutil.TempFile(filePath, handler.Filename)
	if fpath == "" {
		fpath = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload"), handler.Filename)
	}

	// fmt.Println(dir, fpath, handler.Filename)
	fullpath := filepath.Join(dir, fpath, handler.Filename)

	os.MkdirAll(filepath.Dir(fullpath), os.ModePerm)

	if err := ioutil.WriteFile(fullpath, fileBytes, os.ModePerm); err != nil {
		log.Println("Create file error: ", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "✘ Failed: "+err.Error())
		return
	}

	log.Println("Receive file successfully")

	fmt.Fprintf(w, "✔ Succeeded")

}

func delay(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	delay := strings.TrimPrefix(r.URL.Path, "/delay/")
	if r.URL.Path == "/delay" {
		delay = ""
	}

	if delay != "" {
		var err error
		sec, err := strconv.Atoi(delay)
		dur := time.Duration(sec) * time.Second
		if err != nil {
			dur, err = time.ParseDuration(delay)
			if err != nil {
				return
			}
		}
		time.Sleep(dur)
		fmt.Fprintf(w, "(%s later) ", dur)
	}

	fmt.Fprintf(w, "[Headers]:\n")

	for name, headers := range r.Header {
		for _, h := range headers {
			fmt.Fprintf(w, "%v: %v\n", name, h)
		}
	}
}

func echo(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	reg := regexp.MustCompile(`/echo/?(\d*)/?([^/]*)/?(\S*)`) // 中文括号，例如：华南地区（广州） -> 广州
	matches := reg.FindStringSubmatch(r.URL.Path)
	scode := matches[1]
	headers := matches[2]
	content := matches[3]

	for _, header := range strings.Split(headers, ",") {
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		kv := strings.Split(header, "=")
		name := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		w.Header().Add(name, value)
	}

	code, err := strconv.Atoi(scode)
	if err != nil {
		code = 200
	}

	w.WriteHeader(code)

	fmt.Fprintf(w, ">>> %s %s %s\n", r.Method, r.URL, r.Proto)
	fmt.Fprintf(w, ">>> Host: %s\n", r.Host)
	for name, headers := range r.Header {
		for _, h := range headers {
			fmt.Fprintf(w, ">>> %v: %v\n", name, h)
		}
	}
	fmt.Fprintf(w, "\n")

	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if len(body) > 0 {
		fmt.Fprintf(w, "%s\n", string(body))
	}

	fmt.Fprintf(w, "\n<<< %s %d %s\n", r.Proto, code, http.StatusText(code))
	for name, headers := range w.Header() {
		for _, h := range headers {
			fmt.Fprintf(w, "<<< %v: %v\n", name, h)
		}
	}

	fmt.Fprintf(w, "\n%s\n", content)

}

func ip(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	fmt.Fprintf(w, GetLocalIP())
}

func uuid(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		fmt.Fprintf(w, err.Error())
		return
	}

	fmt.Fprintf(w, fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]))
}

func randint(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	maxstr := strings.TrimPrefix(r.URL.Path, "/randint/")
	if r.URL.Path == "/randint" {
		maxstr = ""
	}

	max, err := strconv.Atoi(maxstr)
	if err != nil {
		// fmt.Fprintf(w, err.Error())
		// return
		max = 100
	}

	fmt.Fprintf(w, fmt.Sprintf("%d", rand.Intn(max)))
}

func randstr(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	lengthstr := strings.TrimPrefix(r.URL.Path, "/randstr/")
	if r.URL.Path == "/randstr" {
		lengthstr = ""
	}

	length, err := strconv.Atoi(lengthstr)
	if err != nil {
		// fmt.Fprintf(w, err.Error())
		// return
		length = 12
	}

	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890+=-_@#~,.[]()!%^*$"

	var lr = []rune(letters)
	if length == 0 {
		length = rand.Intn(100) + 1
	}

	b := make([]rune, length)
	for i := range b {
		b[i] = lr[rand.Intn(len(lr))]
	}

	fmt.Fprintf(w, string(b))
}

func ts(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	fmt.Fprintf(w, fmt.Sprintf("%d", time.Now().UnixMilli()))
}

func dt(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	fmt.Fprintf(w, time.Now().Local().Format("2006-01-02 15:04:05"))
}

func healthz(w http.ResponseWriter, r *http.Request) {
	defer func(t time.Time) {
		reqTimes[r.URL.Path]++
		reqSeconds[r.URL.Path] += timeCost(t)
	}(time.Now())

	fmt.Fprintf(w, "healthy")
}

func metrics(w http.ResponseWriter, r *http.Request) {
	metrics := `# HELP gofs_random random number.
# TYPE gofs_random gauge
`
	metrics += fmt.Sprintf("gofs_random{app=\"gofs\"} %d\n", rand.Intn(1000))

	if len(reqSeconds) > 0 {
		metrics += `
# HELP gofs_request_seconds seconds the request spent for each path.
# TYPE gofs_request_seconds counter
`
		for k, v := range reqSeconds {
			metrics += fmt.Sprintf("gofs_request_seconds{app=\"gofs\", path=\"%s\"} %f\n", k, v)
		}
	}

	if len(reqTimes) > 0 {
		metrics += `
# HELP gofs_request_total the request times.
# TYPE gofs_request_total counter
`
		for k, v := range reqTimes {
			metrics += fmt.Sprintf("gofs_request_total{app=\"gofs\", path=\"%s\"} %d\n", k, v)
		}
	}

	fmt.Fprintf(w, metrics)
}

func main() {
	// var dport = flag.String("port", "2333", "server port")
	// var dpath = flag.String("dir", "./", "server path")
	flag.StringVar(&port, "p", "2333", "server port")
	flag.StringVar(&port, "port", "2333", "server port")
	flag.StringVar(&dir, "d", "./", "server path")
	flag.StringVar(&dir, "dir", "./", "server path")

	flag.Parse()

	dir, err := filepath.Abs(dir)
	if err != nil {
		log.Fatal(err)
	}

	host = GetLocalIP()

	http.Handle("/", Gzip(http.FileServer(http.Dir(dir))))

	http.HandleFunc("/upload", upload)
	http.HandleFunc("/upload/", upload)

	http.HandleFunc("/delete", delete)
	http.HandleFunc("/delete/", delete)

	http.HandleFunc("/delay", delay)
	http.HandleFunc("/delay/", delay)

	http.HandleFunc("/echo", echo)
	http.HandleFunc("/echo/", echo)

	http.HandleFunc("/ip", ip)
	http.HandleFunc("/ip/", ip)

	http.HandleFunc("/uuid", uuid)
	http.HandleFunc("/uuid/", uuid)

	http.HandleFunc("/randstr", randstr)
	http.HandleFunc("/randstr/", randstr)

	http.HandleFunc("/randint", randint)
	http.HandleFunc("/randint/", randint)

	http.HandleFunc("/ts", ts)
	http.HandleFunc("/ts/", ts)

	http.HandleFunc("/dt", dt)
	http.HandleFunc("/dt/", dt)

	http.HandleFunc("/healthz", healthz)
	http.HandleFunc("/healthz/", healthz)

	http.HandleFunc("/metrics", metrics)
	http.HandleFunc("/metrics/", metrics)

	log.Printf("serve path: <%s>\n", dir)
	log.Printf("browse url: <0.0.0.0:%s>[%s]\n", port, host)
	log.Printf("upload url: <0.0.0.0:%s/upload>[%s]\n", port, host)
	// log.Println(fmt.Sprintf("starting file server at folder:<%s> address:<0.0.0.0:%s>", dir, port))

	err = http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal(err)
	}

}
