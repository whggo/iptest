package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/geoip2-golang" // 确保这里拼写正确！
)

const (
	requestURL  = "speed.cloudflare.com/cdn-cgi/trace"
	timeout     = 1 * time.Second
	maxDuration = 2 * time.Second
)

var (
	File          = flag.String("file", "ip.txt", "IP地址文件名称,格式为 ip port ,就是IP和端口之间用空格隔开")
	outFile       = flag.String("outfile", "ip.csv", "输出文件名称")
	maxThreads    = flag.Int("max", 100, "并发请求最大协程数")
	speedTest     = flag.Int("speedtest", 5, "下载测速协程数量,设为0禁用测速")
	speedTestURL  = flag.String("url", "speed.cloudflare.com/__down?bytes=50000000", "测速文件地址")
	enableTLS     = flag.Bool("tls", true, "是否启用TLS")
	delay         = flag.Int("delay", 0, "延迟阈值(ms)，默认为0禁用延迟过滤")
	geoDBPath     = flag.String("geodb", "GeoLite2-City.mmdb", "GeoIP数据库文件路径")
	disableGeoIP  = flag.Bool("disable-geoip", false, "禁用GeoIP数据库")
)

type result struct {
	ip          string
	port        int
	dataCenter  string
	locCode     string
	region      string
	city        string
	region_zh   string
	country     string
	city_zh     string
	emoji       string
	latency     string
	tcpDuration time.Duration
}

type speedtestresult struct {
	result
	downloadSpeed float64
}

type location struct {
	Iata      string  `json:"iata"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Cca2      string  `json:"cca2"`
	Region    string  `json:"region"`
	City      string  `json:"city"`
	Region_zh string  `json:"region_zh"`
	Country   string  `json:"country"`
	City_zh   string  `json:"city_zh"`
	Emoji     string  `json:"emoji"`
}

var (
	geoReader *geoip2.Reader
	mu        sync.Mutex
)

func initGeoIP(dbPath string) error {
	if *disableGeoIP {
		fmt.Println("GeoIP已禁用")
		return nil
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("GeoIP数据库文件不存在: %s", dbPath)
	}

	var err error
	geoReader, err = geoip2.Open(dbPath)
	if err != nil {
		return fmt.Errorf("无法打开GeoIP数据库: %v", err)
	}
	fmt.Println("GeoIP数据库加载成功")
	return nil
}

func getIPLocationFromDB(ipStr string) (country, region, city, countryCode string, err error) {
	if geoReader == nil {
		return "", "", "", "", fmt.Errorf("GeoIP数据库未初始化")
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", "", "", "", fmt.Errorf("无效IP地址")
	}

	record, err := geoReader.City(ip)
	if err != nil {
		return "", "", "", "", err
	}

	city = record.City.Names["zh-CN"]
	if city == "" {
		city = record.City.Names["en"]
	}

	if len(record.Subdivisions) > 0 {
		region = record.Subdivisions[0].Names["zh-CN"]
		if region == "" {
			region = record.Subdivisions[0].Names["en"]
		}
	}

	country = record.Country.Names["zh-CN"]
	if country == "" {
		country = record.Country.Names["en"]
	}

	countryCode = record.Country.IsoCode

	return country, region, city, countryCode, nil
}

func increaseMaxOpenFiles() {
	fmt.Println("正在尝试提升文件描述符的上限...")
	cmd := exec.Command("bash", "-c", "ulimit -n 10000")
	_, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("提升文件描述符上限时出现错误: %v\n", err)
	} else {
		fmt.Printf("文件描述符上限已提升!\n")
	}
}

func main() {
	flag.Parse()
	var validCount int32

	startTime := time.Now()
	osType := runtime.GOOS
	if osType == "linux" {
		increaseMaxOpenFiles()
	}

	if err := initGeoIP(*geoDBPath); err != nil {
		fmt.Printf("警告: %v，将使用Cloudflare数据中心信息\n", err)
	} else if geoReader != nil {
		defer geoReader.Close()
	}

	var locations []location
	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("本地 locations.json 不存在\n正在从 https://locations-adw.pages.dev/ 下载 locations.json")
		resp, err := http.Get("https://locations-adw.pages.dev/")
		if err != nil {
			fmt.Printf("无法从URL中获取JSON: %v\n", err)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("无法读取响应体: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("无法解析JSON: %v\n", err)
			return
		}
		file, err := os.Create("locations.json")
		if err != nil {
			fmt.Printf("无法创建文件: %v\n", err)
			return
		}
		defer file.Close()

		_, err = file.Write(body)
		if err != nil {
			fmt.Printf("无法写入文件: %v\n", err)
			return
		}
	} else {
		fmt.Println("本地 locations.json 已存在,无需重新下载")
		file, err := os.Open("locations.json")
		if err != nil {
			fmt.Printf("无法打开文件: %v\n", err)
			return
		}
		defer file.Close()

		body, err := io.ReadAll(file)
		if err != nil {
			fmt.Printf("无法读取文件: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("无法解析JSON: %v\n", err)
			return
		}
	}

	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	ips, err := readIPs(*File)
	if err != nil {
		fmt.Printf("无法从文件中读取 IP: %v\n", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(ips))

	resultChan := make(chan result, len(ips))
	thread := make(chan struct{}, *maxThreads)

	var count int
	total := len(ips)

	for _, ip := range ips {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				count++
				percentage := float64(count) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", count, total, percentage)
				if count == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", count, total, percentage)
				}
			}()

			parts := strings.Fields(ip)
			if len(parts) != 2 {
				fmt.Printf("IP地址格式错误: %s\n", ip)
				return
			}
			ipAddr := parts[0]
			portStr := parts[1]

			port, err := strconv.Atoi(portStr)
			if err != nil {
				fmt.Printf("端口格式错误: %s\n", portStr)
				return
			}

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ipAddr, strconv.Itoa(port)))
			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)
			if *delay > 0 && tcpDuration.Milliseconds() > int64(*delay) {
				return
			}

			start = time.Now()

			client := http.Client{
				Transport: &http.Transport{
					Dial: func(network, addr string) (net.Conn, error) {
						return conn, nil
					},
				},
				Timeout: timeout,
			}

			var protocol string
			if *enableTLS {
				protocol = "https://"
			} else {
				protocol = "http://"
			}
			requestURL := protocol + requestURL

			req, _ := http.NewRequest("GET", requestURL, nil)
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true
			resp, err := client.Do(req)
			if err != nil {
				return
			}

			duration := time.Since(start)
			if duration > maxDuration {
				return
			}

			defer resp.Body.Close()
			buf := &bytes.Buffer{}
			timeout := time.After(maxDuration)
			done := make(chan bool)
			errChan := make(chan error)
			go func() {
				_, err := io.Copy(buf, resp.Body)
				done <- true
				errChan <- err
				if err != nil {
					return
				}
			}()
			select {
			case <-done:
			case <-timeout:
				return
			}

			body := buf
			err = <-errChan
			if err != nil {
				return
			}
			if strings.Contains(body.String(), "uag=Mozilla/5.0") {
				if matches := regexp.MustCompile(`colo=([A-Z]+)[\s\S]*?loc=([A-Z]+)`).FindStringSubmatch(body.String()); len(matches) > 2 {
					dataCenter := matches[1]
					locCode := matches[2]

					atomic.AddInt32(&validCount, 1)

					country, region, city, countryCode, err := getIPLocationFromDB(ipAddr)

					if err == nil && country != "" && !*disableGeoIP {
						emoji := ""
						for _, loc := range locations {
							if loc.Cca2 == countryCode {
								emoji = loc.Emoji
								break
							}
						}
						if emoji == "" {
							if loc, ok := locationMap[dataCenter]; ok {
								emoji = loc.Emoji
							}
						}

						fmt.Printf("发现有效IP %s 端口 %d 位置 %s %s (GeoIP) 延迟 %d 毫秒\n",
							ipAddr, port, city, region, tcpDuration.Milliseconds())

						resultChan <- result{
							ip:          ipAddr,
							port:        port,
							dataCenter:  dataCenter,
							locCode:     countryCode,
							region:      region,
							city:        city,
							region_zh:   region,
							country:     country,
							city_zh:     city,
							emoji:       emoji,
							latency:     fmt.Sprintf("%d ms", tcpDuration.Milliseconds()),
							tcpDuration: tcpDuration,
						}
					} else {
						loc, ok := locationMap[dataCenter]
						if ok {
							fmt.Printf("发现有效IP %s 端口 %d 位置信息 %s (Cloudflare) 延迟 %d 毫秒\n",
								ipAddr, port, loc.City_zh, tcpDuration.Milliseconds())
							resultChan <- result{
								ip:          ipAddr,
								port:        port,
								dataCenter:  dataCenter,
								locCode:     locCode,
								region:      loc.Region,
								city:        loc.City,
								region_zh:   loc.Region_zh,
								country:     loc.Country,
								city_zh:     loc.City_zh,
								emoji:       loc.Emoji,
								latency:     fmt.Sprintf("%d ms", tcpDuration.Milliseconds()),
								tcpDuration: tcpDuration,
							}
						} else {
							fmt.Printf("发现有效IP %s 端口 %d 位置信息未知 延迟 %d 毫秒\n",
								ipAddr, port, tcpDuration.Milliseconds())
							resultChan <- result{
								ip:          ipAddr,
								port:        port,
								dataCenter:  dataCenter,
								locCode:     locCode,
								region:      "",
								city:        "",
								region_zh:   "",
								country:     "",
								city_zh:     "",
								emoji:       "",
								latency:     fmt.Sprintf("%d ms", tcpDuration.Milliseconds()),
								tcpDuration: tcpDuration,
							}
						}
					}
				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		fmt.Print("\033[2J")
		fmt.Println("没有发现有效的IP")
		return
	}
	var results []speedtestresult
	if *speedTest > 0 {
		fmt.Printf("找到符合条件的ip 共%d个\n", atomic.LoadInt32(&validCount))
		fmt.Printf("开始测速\n")
		var wg2 sync.WaitGroup
		wg2.Add(*speedTest)
		count = 0
		total := len(resultChan)
		results = []speedtestresult{}
		for i := 0; i < *speedTest; i++ {
			thread <- struct{}{}
			go func() {
				defer func() {
					<-thread
					wg2.Done()
				}()
				for res := range resultChan {
					downloadSpeed := getDownloadSpeed(res.ip, res.port)
					results = append(results, speedtestresult{result: res, downloadSpeed: downloadSpeed})

					count++
					percentage := float64(count) / float64(total) * 100
					fmt.Printf("已完成: %.2f%%\r", percentage)
					if count == total {
						fmt.Printf("已完成: %.2f%%\n", percentage)
					}
				}
			}()
		}
		wg2.Wait()
	} else {
		for res := range resultChan {
			results = append(results, speedtestresult{result: res})
		}
	}

	if *speedTest > 0 {
		sort.Slice(results, func(i, j int) bool {
			return results[i].downloadSpeed > results[j].downloadSpeed
		})
	} else {
		sort.Slice(results, func(i, j int) bool {
			return results[i].result.tcpDuration < results[j].result.tcpDuration
		})
	}

	file, err := os.Create(*outFile)
	if err != nil {
		fmt.Printf("无法创建文件: %v\n", err)
		return
	}
	defer file.Close()

	_, err = file.WriteString("\xEF\xBB\xBF")
	if err != nil {
		fmt.Printf("写入BOM时出现错误: %v\n", err)
		return
	}

	writer := csv.NewWriter(file)
	if *speedTest > 0 {
		writer.Write([]string{"IP地址", "端口", "TLS", "数据中心", "源IP位置", "地区", "城市", "地区(中文)", "国家", "城市(中文)", "国旗", "网络延迟", "下载速度"})
	} else {
		writer.Write([]string{"IP地址", "端口", "TLS", "数据中心", "源IP位置", "地区", "城市", "地区(中文)", "国家", "城市(中文)", "国旗", "网络延迟"})
	}
	for _, res := range results {
		if *speedTest > 0 {
			writer.Write([]string{
				res.result.ip,
				strconv.Itoa(res.result.port),
				strconv.FormatBool(*enableTLS),
				res.result.dataCenter,
				res.result.locCode,
				res.result.region,
				res.result.city,
				res.result.region_zh,
				res.result.country,
				res.result.city_zh,
				res.result.emoji,
				res.result.latency,
				fmt.Sprintf("%.0f kB/s", res.downloadSpeed),
			})
		} else {
			writer.Write([]string{
				res.result.ip,
				strconv.Itoa(res.result.port),
				strconv.FormatBool(*enableTLS),
				res.result.dataCenter,
				res.result.locCode,
				res.result.region,
				res.result.city,
				res.result.region_zh,
				res.result.country,
				res.result.city_zh,
				res.result.emoji,
				res.result.latency,
			})
		}
	}

	writer.Flush()
	fmt.Print("\033[2J")
	fmt.Printf("有效IP数量: %d | 成功将结果写入文件 %s，耗时 %d秒\n",
		atomic.LoadInt32(&validCount), *outFile, time.Since(startTime)/time.Second)
}

func readIPs(File string) ([]string, error) {
	file, err := os.Open(File)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 {
			fmt.Printf("行格式错误: %s\n", line)
			continue
		}
		ipAddr := parts[0]
		portStr := parts[1]

		port, err := strconv.Atoi(portStr)
		if err != nil {
			fmt.Printf("端口格式错误: %s\n", portStr)
			continue
		}

		ip := fmt.Sprintf("%s %d", ipAddr, port)
		ips = append(ips, ip)
	}
	return ips, scanner.Err()
}

func getDownloadSpeed(ip string, port int) float64 {
	var protocol string
	if *enableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	speedTestURL := protocol + *speedTestURL
	req, _ := http.NewRequest("GET", speedTestURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0
	}
	defer conn.Close()

	fmt.Printf("正在测试IP %s 端口 %d\n", ip, port)
	startTime := time.Now()
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		Timeout: 5 * time.Second,
	}
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("IP %s 端口 %d 测速无效\n", ip, port)
		return 0
	}
	defer resp.Body.Close()

	written, _ := io.Copy(io.Discard, resp.Body)
	duration := time.Since(startTime)
	speed := float64(written) / duration.Seconds() / 1024

	fmt.Printf("IP %s 端口 %d 下载速度 %.0f kB/s\n", ip, port, speed)
	return speed
}
