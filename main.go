package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/go-ping/ping"
	"go.uber.org/zap"
)

var (
	logger      *zap.Logger
	version     bool
	threads     int
	file        string
	pingcount   int
	pingtimeout int
)

func initLogger() {
	logger, _ = zap.NewProduction()
	defer logger.Sync()
}

type Domain struct {
	Name        string
	Latency     int
	Download    int
	DownloadErr bool
}

type ByLatency []Domain

func (a ByLatency) Len() int           { return len(a) }
func (a ByLatency) Less(i, j int) bool { return a[i].Latency < a[j].Latency }
func (a ByLatency) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func main() {
	initLogger()
	defer logger.Sync()

	flag.StringVar(&file, "file", "all.txt", "要读取的域名列表文件")
	flag.BoolVar(&version, "v", false, "输出版本信息")
	flag.IntVar(&threads, "threads", 32, "指定下载测速的线程数量")
	flag.IntVar(&pingcount, "c", 1, "每次 ping 的包次数")
	flag.IntVar(&pingtimeout, "timeout", 1, "ping 的超时时间")
	flag.Parse()

	if version {
		fmt.Print("1.0.0")
		os.Exit(0)
	}

	domains := readDomainsFromFile(file)
	measureLatencyAndDownload(&domains)
	sort.Sort(ByLatency(domains))
	go startGinServer(domains[0].Name)

	// 创建计时器，每隔 10 分钟执行一次测速并更新 domain
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		measureLatencyAndDownload(&domains)
		sort.Sort(ByLatency(domains))
		startGinServer(domains[0].Name)
	}
}

func measureLatencyAndDownload(domains *[]Domain) {
	var wg sync.WaitGroup
	for i := range *domains {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			latency, _ := tping((*domains)[i].Name)
			(*domains)[i].Latency = latency
			if latency != -1 {
				downloadSpeed, err := download((*domains)[i].Name)
				if err != nil {
					logger.Error("无法下载：", zap.Error(err))
					(*domains)[i].DownloadErr = true
					return
				}
				(*domains)[i].Download = downloadSpeed
			}
		}(i)
	}
	wg.Wait()

	excludeDownloadError(domains)
}

func excludeDownloadError(domains *[]Domain) {
	var filtered []Domain
	for _, d := range *domains {
		if !d.DownloadErr {
			filtered = append(filtered, d)
		}
	}
	*domains = filtered
}
func tping(domain string) (int, error) {
	pinger, err := ping.NewPinger(domain)

	if err != nil {
		logger.Error("无法创建pinger：", zap.Error(err))
		return -1, err
	}

	pinger.Count = pingcount
	pinger.Timeout = time.Duration(pingtimeout) * time.Second
	// 提权，防止在某些系统上用不了
	pinger.SetPrivileged(true)

	err = pinger.Run()
	if err != nil {
		logger.Error("无法允许pinger：", zap.Error(err))
		return -1, err
	}

	stats := pinger.Statistics()
	if stats.PacketLoss > 0 {
		logger.Warn("检测到丢包", zap.Float64("packet_loss", stats.PacketLoss))
		return -1, fmt.Errorf("检测到丢包")
	}

	return int(stats.AvgRtt.Milliseconds()), nil
}
func download(domain string) (int, error) {
	var wg sync.WaitGroup
	speedCh := make(chan int)

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := http.Get("https://" + domain + "/project/sevenzip/files/7-Zip/23.01/7zr.exe?viasf=1")
			if err != nil {
				logger.Error("无法下载：", zap.Error(err))
				speedCh <- -1
				return
			}
			defer resp.Body.Close()
			elapsed := time.Since(start).Milliseconds()
			speedCh <- int(elapsed)
		}()
	}

	go func() {
		wg.Wait()
		close(speedCh)
	}()

	var totalSpeed int
	var count int
	for speed := range speedCh {
		if speed != -1 {
			totalSpeed += speed
			count++
		}
	}

	if count == 0 {
		return -1, fmt.Errorf("节点状态异常！")
	}

	return totalSpeed / count, nil
}

func readDomainsFromFile(filename string) []Domain {
	var domains []Domain
	file, err := os.Open(filename)
	if err != nil {
		logger.Error("无法读取文件：", zap.String("filename", filename), zap.Error(err))
		os.Exit(1)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		domains = append(domains, Domain{Name: scanner.Text()})
	}
	if err := scanner.Err(); err != nil {
		logger.Error("文件格式非法：", zap.Error(err))
	}
	return domains
}

func startGinServer(domain string) {
	r := gin.Default()
	r.Use(ginzap.Ginzap(logger, time.RFC3339, true))

	r.GET("/*path", func(c *gin.Context) {
		// 获取用户输入的路径
		originalURI := c.Param("path")

		// 提取原始域名
		originalURI, originalPath := extractDomainAndPath(originalURI)
		fmt.Print(originalURI)
		// 构建替换后的 URI
		redirectURI := buildRedirectURI(originalPath, domain)

		// 执行重定向
		c.Redirect(http.StatusMovedPermanently, redirectURI)
	})

	err := r.Run(":8080")
	if err != nil {
		logger.Error("Web服务启动失败：", zap.Error(err))
	}
}

// 提取原始域名和路径
func extractDomainAndPath(uri string) (string, string) {
	re := regexp.MustCompile(`https://([^/]+)/(.+)`)
	matches := re.FindStringSubmatch(uri)
	if len(matches) != 3 {
		logger.Error("无法提取原始域名和路径：", zap.String("URI", uri))
		return "", ""
	}
	return matches[1], matches[2]
}

// 构建替换后的 URI
func buildRedirectURI(path, domain string) string {
	// 拼接新的 URI
	redirectURI := "https://" + domain + "/" + path + "?viasf=1"
	redirectURI = strings.Replace(redirectURI, "projects", "project", 1)
	redirectURI = strings.Replace(redirectURI, "/download", "", 1)
	redirectURI = strings.Replace(redirectURI, "/files", "", 1)
	return redirectURI
}
