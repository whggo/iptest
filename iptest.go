package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
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
)

const (
	requestURL        = "speed.cloudflare.com/cdn-cgi/trace" // 请求trace URL
	timeout           = 1 * time.Second                      // 超时时间
	maxDuration       = 2 * time.Second                      // 最大持续时间
	cloudflareIPv4URL = "https://www.cloudflare.com/ips-v4/"
	cloudflareIPv6URL = "https://www.cloudflare.com/ips-v6/"
	cloudflareIPFile  = "Cloudflare.txt"
	cloudflareIPv6File = "Cloudflare_ipv6.txt"
	maxIPsPerCIDR     = 8000 // 每个CIDR网段最大测试IP数量
)

var (
	File         = flag.String("file", "ip.txt", "IP地址文件名称,格式为 ip 或 ip/掩码, 例如: 173.245.48.0/20") // IP地址文件名称
	outFile      = flag.String("outfile", "ip.csv", "输出文件名称")                                  // 输出文件名称
	maxThreads   = flag.Int("max", 100, "并发请求最大协程数")                                           // 最大协程数
	speedTest    = flag.Int("speedtest", 5, "下载测速协程数量,设为0禁用测速")                                // 下载测速协程数量
	speedTestURL = flag.String("url", "speed.cloudflare.com/__down?bytes=500000000", "测速文件地址") // 测速文件地址
	enableTLS    = flag.Bool("tls", true, "是否启用TLS")                                           // TLS是否启用
	delay        = flag.Int("delay", 0, "延迟阈值(ms)，默认为0禁用延迟过滤")                               // 默认0，禁用过滤
	region       = flag.String("region", "", "机场码过滤，如HKG,LAX等，多个用逗号分隔")                     // 机场码过滤
	sampleSize   = flag.Int("sample", 50, "从每个CIDR网段中随机测试的IP数量")                           // 从每个CIDR网段中随机测试的IP数量
	defaultPort  = 443                                                                       // 默认端口
)

// Cloudflare IPv6 地址段（内置）
var cloudflareIPv6Ranges = []string{
	// 主要地址段
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

// 机场码映射结构
type AirportCode struct {
	Name    string `json:"name"`
	Region  string `json:"region"`
	Country string `json:"country"`
}

type result struct {
	ip          string        // IP地址
	port        int           // 端口
	dataCenter  string        // 数据中心
	locCode     string        // 源IP位置
	region      string        // 地区
	city        string        // 城市
	region_zh   string        // 地区
	country     string        // 国家
	city_zh     string        // 城市
	emoji       string        // 国旗
	latency     string        // 延迟
	tcpDuration time.Duration // TCP请求延迟
}

type speedtestresult struct {
	result
	downloadSpeed float64 // 下载速度
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

// Cloudflare 数据中心完整机场码映射
var airportCodes = map[string]AirportCode{
	// 亚太地区 - 中国及周边
	"HKG": {"香港", "亚太", "中国香港"},
	"TPE": {"台北", "亚太", "中国台湾"},
	
	// 亚太地区 - 日本
	"NRT": {"东京成田", "亚太", "日本"},
	"KIX": {"大阪", "亚太", "日本"},
	"ITM": {"大阪伊丹", "亚太", "日本"},
	"FUK": {"福冈", "亚太", "日本"},
	
	// 亚太地区 - 韩国
	"ICN": {"首尔仁川", "亚太", "韩国"},
	
	// 亚太地区 - 东南亚
	"SIN": {"新加坡", "亚太", "新加坡"},
	"BKK": {"曼谷", "亚太", "泰国"},
	"HAN": {"河内", "亚太", "越南"},
	"SGN": {"胡志明市", "亚太", "越南"},
	"MNL": {"马尼拉", "亚太", "菲律宾"},
	"CGK": {"雅加达", "亚太", "印度尼西亚"},
	"KUL": {"吉隆坡", "亚太", "马来西亚"},
	"RGN": {"仰光", "亚太", "缅甸"},
	"PNH": {"金边", "亚太", "柬埔寨"},
	
	// 亚太地区 - 南亚
	"BOM": {"孟买", "亚太", "印度"},
	"DEL": {"新德里", "亚太", "印度"},
	"MAA": {"金奈", "亚太", "印度"},
	"BLR": {"班加罗尔", "亚太", "印度"},
	"HYD": {"海得拉巴", "亚太", "印度"},
	"CCU": {"加尔各答", "亚太", "印度"},
	
	// 亚太地区 - 澳洲
	"SYD": {"悉尼", "亚太", "澳大利亚"},
	"MEL": {"墨尔本", "亚太", "澳大利亚"},
	"BNE": {"布里斯班", "亚太", "澳大利亚"},
	"PER": {"珀斯", "亚太", "澳大利亚"},
	"AKL": {"奥克兰", "亚太", "新西兰"},
	
	// 北美地区 - 美国西海岸
	"LAX": {"洛杉矶", "北美", "美国"},
	"SJC": {"圣何塞", "北美", "美国"},
	"SEA": {"西雅图", "北美", "美国"},
	"SFO": {"旧金山", "北美", "美国"},
	"PDX": {"波特兰", "北美", "美国"},
	"SAN": {"圣地亚哥", "北美", "美国"},
	"PHX": {"凤凰城", "北美", "美国"},
	"LAS": {"拉斯维加斯", "北美", "美国"},
	
	// 北美地区 - 美国东海岸
	"EWR": {"纽瓦克", "北美", "美国"},
	"IAD": {"华盛顿", "北美", "美国"},
	"BOS": {"波士顿", "北美", "美国"},
	"PHL": {"费城", "北美", "美国"},
	"ATL": {"亚特兰大", "北美", "美国"},
	"MIA": {"迈阿密", "北美", "美国"},
	"MCO": {"奥兰多", "北美", "美国"},
	
	// 北美地区 - 美国中部
	"ORD": {"芝加哥", "北美", "美国"},
	"DFW": {"达拉斯", "北美", "美国"},
	"IAH": {"休斯顿", "北美", "美国"},
	"DEN": {"丹佛", "北美", "美国"},
	"MSP": {"明尼阿波利斯", "北美", "美国"},
	"DTW": {"底特律", "北美", "美国"},
	"STL": {"圣路易斯", "北美", "美国"},
	"MCI": {"堪萨斯城", "北美", "美国"},
	
	// 北美地区 - 加拿大
	"YYZ": {"多伦多", "北美", "加拿大"},
	"YVR": {"温哥华", "北美", "加拿大"},
	"YUL": {"蒙特利尔", "北美", "加拿大"},
	
	// 欧洲地区 - 西欧
	"LHR": {"伦敦", "欧洲", "英国"},
	"CDG": {"巴黎", "欧洲", "法国"},
	"FRA": {"法兰克福", "欧洲", "德国"},
	"AMS": {"阿姆斯特丹", "欧洲", "荷兰"},
	"BRU": {"布鲁塞尔", "欧洲", "比利时"},
	"ZRH": {"苏黎世", "欧洲", "瑞士"},
	"VIE": {"维也纳", "欧洲", "奥地利"},
	"MUC": {"慕尼黑", "欧洲", "德国"},
	"DUS": {"杜塞尔多夫", "欧洲", "德国"},
	"HAM": {"汉堡", "欧洲", "德国"},
	
	// 欧洲地区 - 南欧
	"MAD": {"马德里", "欧洲", "西班牙"},
	"BCN": {"巴塞罗那", "欧洲", "西班牙"},
	"MXP": {"米兰", "欧洲", "意大利"},
	"FCO": {"罗马", "欧洲", "意大利"},
	"ATH": {"雅典", "欧洲", "希腊"},
	"LIS": {"里斯本", "欧洲", "葡萄牙"},
	
	// 欧洲地区 - 北欧
	"ARN": {"斯德哥尔摩", "欧洲", "瑞典"},
	"CPH": {"哥本哈根", "欧洲", "丹麦"},
	"OSL": {"奥斯陆", "欧洲", "挪威"},
	"HEL": {"赫尔辛基", "欧洲", "芬兰"},
	
	// 欧洲地区 - 东欧
	"WAW": {"华沙", "欧洲", "波兰"},
	"PRG": {"布拉格", "欧洲", "捷克"},
	"BUD": {"布达佩斯", "欧洲", "匈牙利"},
	"OTP": {"布加勒斯特", "欧洲", "罗马尼亚"},
	"SOF": {"索非亚", "欧洲", "保加利亚"},
	
	// 中东地区
	"DXB": {"迪拜", "中东", "阿联酋"},
	"TLV": {"特拉维夫", "中东", "以色列"},
	"BAH": {"巴林", "中东", "巴林"},
	"AMM": {"安曼", "中东", "约旦"},
	"KWI": {"科威特", "中东", "科威特"},
	"DOH": {"多哈", "中东", "卡塔尔"},
	"MCT": {"马斯喀特", "中东", "阿曼"},
	
	// 南美地区
	"GRU": {"圣保罗", "南美", "巴西"},
	"GIG": {"里约热内卢", "南美", "巴西"},
	"EZE": {"布宜诺斯艾利斯", "南美", "阿根廷"},
	"BOG": {"波哥大", "南美", "哥伦比亚"},
	"LIM": {"利马", "南美", "秘鲁"},
	"SCL": {"圣地亚哥", "南美", "智利"},
	
	// 非洲地区
	"JNB": {"约翰内斯堡", "非洲", "南非"},
	"CPT": {"开普敦", "非洲", "南非"},
	"CAI": {"开罗", "非洲", "埃及"},
	"LOS": {"拉各斯", "非洲", "尼日利亚"},
	"NBO": {"内罗毕", "非洲", "肯尼亚"},
	"ACC": {"阿克拉", "非洲", "加纳"},
}

// 从Cloudflare官方获取IP范围
func getCloudflareIPRanges() ([]string, error) {
	fmt.Println("正在从Cloudflare官方获取IP范围...")
	
	var allRanges []string
	
	// 获取IPv4范围
	ipv4Ranges, err := getIPRangesFromURL(cloudflareIPv4URL)
	if err != nil {
		return nil, fmt.Errorf("获取IPv4范围失败: %v", err)
	}
	allRanges = append(allRanges, ipv4Ranges...)
	
	// 获取IPv6范围
	ipv6Ranges, err := getIPRangesFromURL(cloudflareIPv6URL)
	if err != nil {
		fmt.Printf("获取IPv6范围失败，使用内置IPv6范围: %v\n", err)
		// 使用内置IPv6范围作为备选
		allRanges = append(allRanges, cloudflareIPv6Ranges...)
	} else {
		allRanges = append(allRanges, ipv6Ranges...)
	}
	
	// 保存到本地文件
	if err := saveIPRangesToFile(allRanges, cloudflareIPFile); err != nil {
		fmt.Printf("保存IP范围到文件失败: %v\n", err)
	}
	
	fmt.Printf("成功获取 %d 个Cloudflare IP范围（IPv4: %d, IPv6: %d）\n", 
		len(allRanges), len(ipv4Ranges), len(ipv6Ranges))
	
	return allRanges, nil
}

// 从URL获取IP范围
func getIPRangesFromURL(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var ranges []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			ranges = append(ranges, line)
		}
	}
	
	return ranges, nil
}

// 保存IP范围到文件
func saveIPRangesToFile(ranges []string, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	
	for _, r := range ranges {
		_, err := file.WriteString(r + "\n")
		if err != nil {
			return err
		}
	}
	
	fmt.Printf("已保存 %d 个IP范围到 %s\n", len(ranges), filename)
	return nil
}

// 从本地文件读取IP范围
func getIPRangesFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	
	var ranges []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			ranges = append(ranges, line)
		}
	}
	
	return ranges, scanner.Err()
}

// 根据机场码获取对应的IP网段
func getIPRangesByAirportCodes(codes []string) ([]string, error) {
	// 首先尝试从本地文件读取
	if _, err := os.Stat(cloudflareIPFile); err == nil {
		fmt.Println("从本地文件读取Cloudflare IP范围...")
		ranges, err := getIPRangesFromFile(cloudflareIPFile)
		if err == nil && len(ranges) > 0 {
			fmt.Printf("从本地文件读取到 %d 个IP范围\n", len(ranges))
			return ranges, nil
		}
	}
	
	// 从Cloudflare官方获取
	return getCloudflareIPRanges()
}

// 从CIDR网段中智能采样IP地址
func sampleIPsFromCIDR(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	// 计算网段中的IP数量
	ones, bits := ipnet.Mask.Size()
	totalIPs := 1 << (bits - ones)
	
	fmt.Printf("网段 %s 包含 %d 个IP地址\n", cidr, totalIPs)

	var ips []string
	
	if totalIPs <= maxIPsPerCIDR {
		// 如果IP数量小于等于8000，测试所有IP
		ip := make(net.IP, len(ipnet.IP))
		copy(ip, ipnet.IP)
		
		// 跳过网络地址
		inc(ip)
		
		for i := 0; i < totalIPs-2 && ipnet.Contains(ip); i++ {
			ips = append(ips, ip.String())
			inc(ip)
		}
		fmt.Printf("网段 %s 扩展为 %d 个IP\n", cidr, len(ips))
	} else {
		// 如果IP数量大于8000，使用高效的随机采样算法
		fmt.Printf("网段过大，随机采样 %d 个IP进行测试\n", maxIPsPerCIDR)
		ips = efficientRandomSample(ipnet, maxIPsPerCIDR)
		fmt.Printf("网段 %s 采样为 %d 个IP\n", cidr, len(ips))
	}

	return ips, nil
}

// 高效的随机采样算法
func efficientRandomSample(ipnet *net.IPNet, sampleSize int) []string {
	ones, bits := ipnet.Mask.Size()
	totalIPs := 1 << (bits - ones)
	
	// 如果采样数量接近总数，直接使用所有IP
	if sampleSize >= totalIPs-10 {
		return getAllIPsFromCIDR(ipnet)
	}
	
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	ips := make([]string, 0, sampleSize)
	
	// 使用布隆过滤器风格的简单去重
	seen := make(map[uint32]bool)
	
	for len(ips) < sampleSize {
		// 生成随机偏移量
		offset := r.Uint32() % uint32(totalIPs)
		
		// 跳过网络地址和广播地址
		if offset == 0 || offset == uint32(totalIPs-1) {
			continue
		}
		
		// 检查是否已经选择过这个偏移量
		if seen[offset] {
			continue
		}
		seen[offset] = true
		
		// 根据偏移量计算IP
		ip := calculateIPFromOffset(ipnet.IP, ipnet.Mask, offset)
		if ip != nil {
			ips = append(ips, ip.String())
		}
		
		// 防止无限循环
		if len(seen) >= totalIPs-2 {
			break
		}
	}
	
	return ips
}

// 根据偏移量计算IP地址
func calculateIPFromOffset(baseIP net.IP, mask net.IPMask, offset uint32) net.IP {
	ip := make(net.IP, len(baseIP))
	copy(ip, baseIP)
	
	// 对于IPv4
	if len(ip) == net.IPv4len {
		// 将偏移量加到IP上
		val := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		val += offset
		ip[0] = byte(val >> 24)
		ip[1] = byte(val >> 16)
		ip[2] = byte(val >> 8)
		ip[3] = byte(val)
		return ip
	}
	
	// 对于IPv6（简化处理，只采样少量）
	if len(ip) == net.IPv6len {
		// 简化：只在最后16位进行随机
		ip[14] = byte(offset >> 8)
		ip[15] = byte(offset)
		return ip
	}
	
	return nil
}

// 获取CIDR中的所有IP（用于小网段）
func getAllIPsFromCIDR(ipnet *net.IPNet) []string {
	var ips []string
	ip := make(net.IP, len(ipnet.IP))
	copy(ip, ipnet.IP)
	
	// 跳过网络地址
	inc(ip)
	
	for ipnet.Contains(ip) {
		ips = append(ips, ip.String())
		inc(ip)
	}
	
	// 移除最后一个（广播地址）
	if len(ips) > 0 {
		ips = ips[:len(ips)-1]
	}
	
	return ips
}

// 尝试提升文件描述符的上限
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
	var validCount int32 // 有效IP计数器

	startTime := time.Now()
	osType := runtime.GOOS
	if osType == "linux" {
		increaseMaxOpenFiles()
	}

	// 解析机场码过滤
	var regionFilter map[string]bool
	var selectedCodes []string
	if *region != "" {
		regionFilter = make(map[string]bool)
		regions := strings.Split(*region, ",")
		for _, r := range regions {
			code := strings.TrimSpace(strings.ToUpper(r))
			if _, exists := airportCodes[code]; exists {
				regionFilter[code] = true
				selectedCodes = append(selectedCodes, code)
				fmt.Printf("已选择机场: %s - %s\n", code, airportCodes[code].Name)
			} else {
				fmt.Printf("警告: 未知的机场码 %s，已跳过\n", code)
			}
		}
		if len(regionFilter) == 0 {
			fmt.Println("错误: 没有有效的机场码")
			return
		}
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
		file, err := os.Open("locations.json)
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

	var ips []string
	var err error

	// 如果指定了机场码，直接从Cloudflare获取对应IP范围并扩展
	if *region != "" {
		fmt.Printf("正在根据机场码 %v 获取Cloudflare IP范围...\n", selectedCodes)
		ipRanges, err := getIPRangesByAirportCodes(selectedCodes)
		if err != nil {
			fmt.Printf("获取Cloudflare IP范围失败: %v\n", err)
			fmt.Println("回退到使用文件中的IP列表")
			ips, err = readIPs(*File)
			if err != nil {
				fmt.Printf("无法从文件中读取 IP: %v\n", err)
				return
			}
		} else {
			// 将CIDR网段扩展为具体的IP地址进行测试
			fmt.Printf("正在从 %d 个CIDR网段中扩展IP地址...\n", len(ipRanges))
			totalIPs := 0
			for _, ipRange := range ipRanges {
				if strings.Contains(ipRange, "/") {
					sampledIPs, err := sampleIPsFromCIDR(ipRange)
					if err != nil {
						fmt.Printf("扩展CIDR网段 %s 失败: %v\n", ipRange, err)
						continue
					}
					for _, ip := range sampledIPs {
						ips = append(ips, fmt.Sprintf("%s %d", ip, defaultPort))
					}
					totalIPs += len(sampledIPs)
				} else {
					// 单个IP地址
					ips = append(ips, fmt.Sprintf("%s %d", ipRange, defaultPort))
					totalIPs++
				}
			}
			fmt.Printf("总共扩展出 %d 个IP地址进行测试\n", totalIPs)
		}
	} else {
		// 使用文件中的IP列表
		ips, err = readIPs(*File)
		if err != nil {
			fmt.Printf("无法从文件中读取 IP: %v\n", err)
			return
		}
	}

	fmt.Printf("总共需要测试 %d 个IP地址\n", len(ips))

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
			if len(parts) < 1 {
				fmt.Printf("IP地址格式错误: %s\n", ip)
				return
			}
			
			ipAddr := parts[0]
			port := defaultPort // 默认使用443端口
			
			// 如果提供了端口，则使用提供的端口
			if len(parts) >= 2 {
				portStr := parts[1]
				if p, err := strconv.Atoi(portStr); err == nil {
					port = p
				} else {
					fmt.Printf("端口格式错误: %s，使用默认端口 %d\n", portStr, defaultPort)
				}
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
				return // 超过延迟阈值直接返回（仅在delay>0时生效）
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

			// 添加用户代理
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
			// 创建一个读取操作的超时
			timeout := time.After(maxDuration)
			// 使用一个 goroutine 来读取响应体
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
			// 等待读取操作完成或者超时
			select {
			case <-done:
				// 读取操作完成
			case <-timeout:
				// 读取操作超时
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
					
					// 机场码过滤
					if regionFilter != nil {
						if !regionFilter[dataCenter] {
							return // 不在过滤列表中，直接返回
						}
					}
					
					loc, ok := locationMap[dataCenter]
					// 记录通过延迟检查的有效IP
					atomic.AddInt32(&validCount, 1)
					if ok {
						fmt.Printf("发现有效IP %s 端口 %d 位置信息 %s 延迟 %d 毫秒\n", ipAddr, port, loc.City_zh, tcpDuration.Milliseconds())
						resultChan <- result{ipAddr, port, dataCenter, locCode, loc.Region, loc.City, loc.Region_zh, loc.Country, loc.City_zh, loc.Emoji, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					} else {
						fmt.Printf("发现有效IP %s 端口 %d 位置信息未知 延迟 %d 毫秒\n", ipAddr, port, tcpDuration.Milliseconds())
						resultChan <- result{ipAddr, port, dataCenter, locCode, "", "", "", "", "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					}
				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		// 清除输出内容
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
						fmt.Printf("已完成: %.2f%%\033[0\n", percentage)
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

	// 写入UTF-8 BOM防止中文乱码
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
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.dataCenter, res.result.locCode, res.result.region, res.result.city, res.result.region_zh, res.result.country, res.result.city_zh, res.result.emoji, res.result.latency, fmt.Sprintf("%.0f kB/s", res.downloadSpeed)})
		} else {
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.dataCenter, res.result.locCode, res.result.region, res.result.city, res.result.region_zh, res.result.country, res.result.city_zh, res.result.emoji, res.result.latency})
		}
	}

	writer.Flush()
	// 清除输出内容
	fmt.Print("\033[2J")
	fmt.Printf("有效IP数量: %d | 成功将结果写入文件 %s，耗时 %d秒\n", atomic.LoadInt32(&validCount), *outFile, time.Since(startTime)/time.Second)
}

// 从CIDR格式解析IP地址范围
func expandCIDR(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip := ipnet.IP.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}
	// 移除网络地址和广播地址
	if len(ips) > 2 {
		return ips[1 : len(ips)-1], nil
	}
	return ips, nil
}

// IP地址递增
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// 从文件中读取IP地址和端口
func readIPs(File string) ([]string, error) {
	file, err := os.Open(File)
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
		if len(parts) == 0 {
			continue
		}

		ipAddr := parts[0]
		
		// 检查是否是CIDR格式
		if strings.Contains(ipAddr, "/") {
			expandedIPs, err := expandCIDR(ipAddr)
			if err != nil {
				fmt.Printf("CIDR格式错误: %s, 错误: %v\n", ipAddr, err)
				continue
			}
			
			// 如果有指定端口，使用指定端口，否则使用默认端口
			port := defaultPort
			if len(parts) >= 2 {
				if p, err := strconv.Atoi(parts[1]); err == nil {
					port = p
				}
			}
			
			for _, expandedIP := range expandedIPs {
				ips = append(ips, fmt.Sprintf("%s %d", expandedIP, port))
			}
		} else {
			// 单个IP地址
			port := defaultPort
			if len(parts) >= 2 {
				if p, err := strconv.Atoi(parts[1]); err == nil {
					port = p
				} else {
					fmt.Printf("端口格式错误: %s，使用默认端口 %d\n", parts[1], defaultPort)
				}
			}
			ips = append(ips, fmt.Sprintf("%s %d", ipAddr, port))
		}
	}
	return ips, scanner.Err()
}

// 测速函数
func getDownloadSpeed(ip string, port int) float64 {
	var protocol string
	if *enableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	speedTestURL := protocol + *speedTestURL
	// 创建请求
	req, _ := http.NewRequest("GET", speedTestURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	// 创建TCP连接
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
	// 创建HTTP客户端
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		//设置单个IP测速最长时间为5秒
		Timeout: 5 * time.Second,
	}
	// 发送请求
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("IP %s 端口 %d 测速无效\n", ip, port)
		return 0
	}
	defer resp.Body.Close()

	// 复制响应体到/dev/null，并计算下载速度
	written, _ := io.Copy(io.Discard, resp.Body)
	duration := time.Since(startTime)
	speed := float64(written) / duration.Seconds() / 1024

	// 输出结果
	fmt.Printf("IP %s 端口 %d 下载速度 %.0f kB/s\n", ip, port, speed)
	return speed
}
