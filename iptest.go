package main

import (
	"bufio"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	timeout     = 2 * time.Second
	maxDuration = 5 * time.Second
)

var (
	File         = flag.String("file", "ip.txt", "IP地址文件名称,格式为 ip port")
	outFile      = flag.String("outfile", "ip.csv", "输出文件名称")
	maxThreads   = flag.Int("max", 100, "并发请求最大协程数")
	speedTest    = flag.Int("speedtest", 5, "下载测速协程数量,设为0禁用测速")
	speedTestURL = flag.String("url", "speed.cloudflare.com/__down?bytes=50000000", "测速文件地址")
	enableTLS    = flag.Bool("tls", true, "是否启用TLS")
	delay        = flag.Int("delay", 0, "延迟阈值(ms)，默认为0禁用延迟过滤")
)

type IPInfo struct {
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Region      string  `json:"region"`
	RegionCode  string  `json:"region_code"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	ISP         string  `json:"isp"`
	Organization string `json:"org"`
	Timezone    string  `json:"timezone"`
}

type Result struct {
	IP            string
	Port          int
	Country       string
	Region        string
	City          string
	ISP           string
	Latency       string
	TCPDuration   time.Duration
	DownloadSpeed float64
}

// IP地理位置查询（使用多个备用API）
func getIPInfo(ip string) (*IPInfo, error) {
	// 尝试多个API以提高准确性
	apis := []string{
		// 主要使用ip-api.com，对中国IP较准确
		fmt.Sprintf("http://ip-api.com/json/%s?fields=status,message,country,countryCode,region,regionName,city,lat,lon,isp,org,timezone", ip),
		// 备用API
		fmt.Sprintf("https://ipinfo.io/%s/json", ip),
	}

	for _, apiURL := range apis {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(apiURL)
		if err != nil {
			continue
		}
		
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		var info IPInfo
		if err := json.Unmarshal(body, &info); err != nil {
			continue
		}

		// 检查是否成功获取信息
		if info.Country != "" || info.City != "" {
			info.IP = ip
			return &info, nil
		}
	}

	return nil, fmt.Errorf("无法获取IP %s 的地理位置信息", ip)
}

func increaseMaxOpenFiles() {
	if runtime.GOOS == "linux" {
		fmt.Println("正在优化系统资源限制...")
		cmd := exec.Command("bash", "-c", "ulimit -n 10000")
		if err := cmd.Run(); err != nil {
			fmt.Printf("优化资源限制失败: %v\n", err)
		}
	}
}

func readIPs(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			fmt.Printf("跳过格式错误行: %s\n", line)
			continue
		}
		
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Printf("跳过无效端口: %s\n", parts[1])
			continue
		}
		
		ips = append(ips, fmt.Sprintf("%s %d", parts[0], port))
	}
	
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	
	return ips, nil
}

// 修复1: 延迟测试 - 直接使用IP避免DNS解析，并包含TLS握手
func testTCPConnection(ip string, port int) (time.Duration, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	
	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	
	// 如果启用TLS，测量TLS握手时间
	if *enableTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "cloudflare.com",
		})
		if err := tlsConn.Handshake(); err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}
	
	return time.Since(start), nil
}

// 修复2: HTTP完整请求延迟测试（可选）
func testHTTPLatency(ip string, port int) (time.Duration, error) {
	protocol := "http://"
	if *enableTLS {
		protocol = "https://"
	}
	
	// 使用HEAD请求测试延迟
	url := fmt.Sprintf("%s%s:%d/", protocol, ip, port)
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	
	return time.Since(start), nil
}

// 修复3: 速度测试优化
func testSpeed(ip string, port int) float64 {
	if *speedTest <= 0 {
		return 0
	}
	
	protocol := "http://"
	if *enableTLS {
		protocol = "https://"
	}
	
	// 构建完整的URL
	url := protocol + *speedTestURL
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Connection", "close")
	
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Timeout: 10 * time.Second,
	}

	startTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	written, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0
	}
	
	duration := time.Since(startTime)
	if duration.Seconds() == 0 {
		return 0
	}
	
	// 计算速度 (KB/s)
	speed := float64(written) / duration.Seconds() / 1024
	return speed
}

// 修复4: 主要处理逻辑优化
func processIP(ipLine string, results chan<- Result, wg *sync.WaitGroup, sem chan struct{}) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("处理IP %s 时发生panic: %v\n", ipLine, r)
		}
	}()
	
	sem <- struct{}{}
	defer func() { <-sem }()

	parts := strings.Fields(ipLine)
	if len(parts) != 2 {
		return
	}
	
	ipAddr := parts[0]
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}

	// 1. 测量TCP连接延迟（包含TLS握手）
	tcpDuration, err := testTCPConnection(ipAddr, port)
	if err != nil {
		return
	}
	
	// 2. 测量HTTP完整请求延迟（可选，用于更准确的延迟）
	var httpDuration time.Duration
	var effectiveLatency time.Duration
	
	// 如果启用了延迟过滤或速度测试，进行HTTP延迟测试
	if *delay > 0 || *speedTest > 0 {
		httpDuration, _ = testHTTPLatency(ipAddr, port)
	}
	
	// 选择延迟值：优先使用HTTP延迟（更准确），否则使用TCP延迟
	if httpDuration > 0 && httpDuration < maxDuration {
		effectiveLatency = httpDuration
	} else {
		effectiveLatency = tcpDuration
	}
	
	// 延迟过滤
	if *delay > 0 && effectiveLatency.Milliseconds() > int64(*delay) {
		return
	}

	// 3. 获取地理位置信息
	ipInfo, err := getIPInfo(ipAddr)
	if err != nil {
		// 如果无法获取地理位置，仍然记录IP和延迟
		results <- Result{
			IP:          ipAddr,
			Port:        port,
			Country:     "未知",
			Region:      "未知",
			City:        "未知",
			ISP:         "未知",
			Latency:     fmt.Sprintf("%d ms", effectiveLatency.Milliseconds()),
			TCPDuration: effectiveLatency,
		}
		return
	}

	// 4. 速度测试（如果启用）
	var downloadSpeed float64
	if *speedTest > 0 {
		downloadSpeed = testSpeed(ipAddr, port)
	}

	result := Result{
		IP:            ipAddr,
		Port:          port,
		Country:       ipInfo.Country,
		Region:        ipInfo.Region,
		City:          ipInfo.City,
		ISP:           ipInfo.ISP,
		Latency:       fmt.Sprintf("%d ms", effectiveLatency.Milliseconds()),
		TCPDuration:   effectiveLatency,
		DownloadSpeed: downloadSpeed,
	}
	
	results <- result
	
	// 输出结果
	if downloadSpeed > 0 {
		fmt.Printf("✓ 有效IP: %s:%d | 位置: %s %s %s | 延迟: %dms | 速度: %.0f KB/s\n",
			ipAddr, port, ipInfo.Country, ipInfo.Region, ipInfo.City,
			effectiveLatency.Milliseconds(), downloadSpeed)
	} else {
		fmt.Printf("✓ 有效IP: %s:%d | 位置: %s %s %s | 延迟: %dms\n",
			ipAddr, port, ipInfo.Country, ipInfo.Region, ipInfo.City,
			effectiveLatency.Milliseconds())
	}
}

func main() {
	flag.Parse()
	
	startTime := time.Now()
	increaseMaxOpenFiles()

	// 读取IP列表
	ips, err := readIPs(*File)
	if err != nil {
		fmt.Printf("读取IP文件失败: %v\n", err)
		return
	}
	
	if len(ips) == 0 {
		fmt.Println("没有找到有效的IP地址")
		return
	}
	
	fmt.Printf("共加载 %d 个IP地址，开始测试...\n", len(ips))
	fmt.Printf("配置: 并发数=%d, 测速线程=%d, TLS=%v, 延迟阈值=%dms\n", 
		*maxThreads, *speedTest, *enableTLS, *delay)

	var wg sync.WaitGroup
	results := make(chan Result, len(ips))
	sem := make(chan struct{}, *maxThreads)
	
	var processed int32
	total := len(ips)

	// 启动工作协程
	for _, ip := range ips {
		wg.Add(1)
		go processIP(ip, results, &wg, sem)
		
		// 显示进度
		atomic.AddInt32(&processed, 1)
		if atomic.LoadInt32(&processed)%10 == 0 || atomic.LoadInt32(&processed) == int32(total) {
			fmt.Printf("进度: %d/%d (%.1f%%)\n", 
				atomic.LoadInt32(&processed), total, 
				float64(atomic.LoadInt32(&processed))/float64(total)*100)
		}
	}

	// 等待所有协程完成
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果
	var allResults []Result
	var validCount int
	var totalLatency time.Duration
	
	for result := range results {
		allResults = append(allResults, result)
		validCount++
		totalLatency += result.TCPDuration
	}

	if len(allResults) == 0 {
		fmt.Println("没有发现有效的IP地址")
		return
	}

	// 计算平均延迟
	avgLatency := totalLatency / time.Duration(validCount)

	// 排序结果
	if *speedTest > 0 {
		// 按速度降序排序
		sort.Slice(allResults, func(i, j int) bool {
			return allResults[i].DownloadSpeed > allResults[j].DownloadSpeed
		})
	} else {
		// 按延迟升序排序
		sort.Slice(allResults, func(i, j int) bool {
			return allResults[i].TCPDuration < allResults[j].TCPDuration
		})
	}

	// 输出CSV文件
	if err := writeCSV(allResults, *outFile); err != nil {
		fmt.Printf("写入CSV文件失败: %v\n", err)
		return
	}

	// 输出统计信息
	fmt.Printf("\n✅ 测试完成!\n")
	fmt.Printf("有效IP数量: %d/%d\n", validCount, total)
	fmt.Printf("平均延迟: %d ms\n", avgLatency.Milliseconds())
	fmt.Printf("最快延迟: %d ms\n", allResults[0].TCPDuration.Milliseconds())
	if *speedTest > 0 {
		fmt.Printf("最快速度: %.0f KB/s (%.2f MB/s)\n", 
			allResults[0].DownloadSpeed, 
			allResults[0].DownloadSpeed/1024)
	}
	fmt.Printf("结果已保存到: %s\n", *outFile)
	fmt.Printf("总耗时: %.2f 秒\n", time.Since(startTime).Seconds())
}

func writeCSV(results []Result, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// 写入UTF-8 BOM
	if _, err := file.WriteString("\xEF\xBB\xBF"); err != nil {
		return err
	}

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// 写入表头
	header := []string{"IP地址", "端口", "TLS", "国家", "地区", "城市", "ISP", "网络延迟(ms)"}
	if *speedTest > 0 {
		header = append(header, "下载速度(KB/s)", "下载速度(MB/s)")
	}
	if err := writer.Write(header); err != nil {
		return err
	}

	// 写入数据
	for _, r := range results {
		row := []string{
			r.IP,
			strconv.Itoa(r.Port),
			strconv.FormatBool(*enableTLS),
			r.Country,
			r.Region,
			r.City,
			r.ISP,
			r.Latency,
		}
		if *speedTest > 0 {
			speedMB := r.DownloadSpeed / 1024
			row = append(row, fmt.Sprintf("%.0f", r.DownloadSpeed))
			row = append(row, fmt.Sprintf("%.2f", speedMB))
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}

	return nil
}
