package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Area struct {
	ID            int     `json:"id"`
	AreaName      string  `json:"areaName"`
	Longitude     float64 `json:"longitude,string"`
	Latitude      float64 `json:"latitude,string"`
	DetailAddress string  `json:"detailAddress"`
	TotalDevice   int     `json:"totalDeviceNum"`
	FreeDevice    int     `json:"freeDeviceNum"`
	WaitDuration  int     `json:"waitDuration"`
}

type ApiResponse struct {
	Code int `json:"code"`
	Data struct {
		Records []Area `json:"records"`
	} `json:"data"`
}

type Center struct {
	Name string
	Lat  float64
	Lng  float64
}

// 配置结构体
type Config struct {
	Authorization string        `json:"authorization"`
	Interval      time.Duration `json:"interval"`
	Duration      time.Duration `json:"duration"`
	Export        bool          `json:"export"`
	MaxBlocks     int           `json:"maxBlocks"`
	OutputDB      string        `json:"outputDB"`
	OutputExcel   string        `json:"outputExcel"`
}

// 配置文件路径
const configFile = "config.json"

const apiUrl = "https://toc.lemobar.com/api-toc/api/area/near?current=1&size=20&longitude=%f&latitude=%f&type=0"

var defaultHeaders = map[string]string{
	"content-type":    "application/x-www-form-urlencoded",
	"Cache-Control":   "no-cache",
	"p":               "202507",
	"lan":             "zh-Hans",
	"x-session-id":    "31751347839278791807",
	"charset":         "utf-8",
	"Referer":         "https://servicewechat.com/wxadc480e27684767a/446/page-frame.html",
	"User-Agent":      "Mozilla/5.0 (Linux; Android 11; Pixel 3a...) Weixin NetType/WIFI Language/zh_CN ABI/arm64 MiniProgramEnv/android",
	"Accept-Encoding": "gzip, deflate, br",
}

var client = &http.Client{Timeout: 8 * time.Second}

func fetchAreas(lat, lng float64, config *Config) ([]Area, error) {
	url := fmt.Sprintf(apiUrl, lng, lat)
	req, _ := http.NewRequest("GET", url, nil)

	// 设置请求头
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", config.Authorization)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result ApiResponse
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil || result.Code != 200 {
		return nil, fmt.Errorf("API decode failed or code!=200")
	}

	return result.Data.Records, nil
}

func spiralScan(db *sql.DB, center Center, config *Config, wg *sync.WaitGroup) {
	defer wg.Done()
	step := 0.03
	insertStmt, err := db.Prepare(`INSERT OR IGNORE INTO lemobar_areas (area_id, area_name, detail_address, latitude, longitude, total_device_num, free_device_num, wait_duration) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		log.Printf("[%s] Failed to prepare insert: %v\n", center.Name, err)
		return
	}
	defer insertStmt.Close()

	type Dir struct{ dx, dy int }
	dirs := []Dir{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}
	x, y, dirIdx, dist := 0, 0, 0, 1
	scanned := 0
	startTime := time.Now()

	for scanned < config.MaxBlocks && time.Since(startTime) < config.Duration {
		for i := 0; i < 2; i++ {
			for j := 0; j < dist; j++ {
				if scanned >= config.MaxBlocks || time.Since(startTime) >= config.Duration {
					return
				}
				lng := center.Lng + float64(x)*step
				lat := center.Lat + float64(y)*step
				areas, err := fetchAreas(lat, lng, config)
				if err == nil {
					for _, a := range areas {
						_, _ = insertStmt.Exec(a.ID, a.AreaName, a.DetailAddress, a.Latitude, a.Longitude, a.TotalDevice, a.FreeDevice, a.WaitDuration)
					}
					log.Printf("[%s@%d] (%.4f, %.4f) → %d 点\n", center.Name, scanned, lat, lng, len(areas))
				} else {
					log.Printf("[%s@%d] ✗ (%.4f, %.4f) - %v\n", center.Name, scanned, lat, lng, err)
				}
				x += dirs[dirIdx].dx
				y += dirs[dirIdx].dy
				scanned++
				time.Sleep(config.Interval)
			}
			dirIdx = (dirIdx + 1) % 4
		}
		dist++
	}

	log.Printf("[%s] 扫描完成，共处理 %d 个位置，耗时 %.2f 分钟\n", center.Name, scanned, time.Since(startTime).Minutes())
}

// 导出数据到Excel文件
func exportToExcel(config *Config) error {
	fmt.Printf("📂 正在打开数据库: %s\n", config.OutputDB)

	db, err := sql.Open("sqlite3", config.OutputDB)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// 检查表是否存在
	var tableExists int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lemobar_areas'").Scan(&tableExists)
	if err != nil {
		return fmt.Errorf("检查数据库表失败: %v", err)
	}

	if tableExists == 0 {
		return fmt.Errorf("数据库表不存在，请先采集数据")
	}

	// 检查数据数量
	var totalCount int
	err = db.QueryRow("SELECT COUNT(*) FROM lemobar_areas").Scan(&totalCount)
	if err != nil {
		return fmt.Errorf("统计数据数量失败: %v", err)
	}

	if totalCount == 0 {
		return fmt.Errorf("数据库中没有数据，请先采集数据")
	}

	fmt.Printf("📊 找到 %d 条记录，开始导出...\n", totalCount)

	rows, err := db.Query(`SELECT area_id, area_name, detail_address, latitude, longitude, total_device_num, free_device_num, wait_duration FROM lemobar_areas`)
	if err != nil {
		return fmt.Errorf("查询数据失败: %v", err)
	}
	defer rows.Close()

	// 获取文件的绝对路径
	absPath, err := filepath.Abs(config.OutputExcel)
	if err != nil {
		absPath = config.OutputExcel
	}

	fmt.Printf("📝 正在创建导出文件: %s\n", absPath)

	// 创建Excel文件
	file, err := os.Create(config.OutputExcel)
	if err != nil {
		return fmt.Errorf("创建Excel文件失败: %v", err)
	}
	defer file.Close()

	// 写入CSV格式（简化的Excel）
	headers := "area_id,area_name,detail_address,latitude,longitude,total_device_num,free_device_num,wait_duration\n"
	file.WriteString(headers)

	count := 0
	for rows.Next() {
		var id int
		var name, address string
		var lat, lng float64
		var total, free, wait int

		err := rows.Scan(&id, &name, &address, &lat, &lng, &total, &free, &wait)
		if err != nil {
			log.Printf("扫描行数据失败: %v", err)
			continue
		}

		line := fmt.Sprintf("%d,\"%s\",\"%s\",%.6f,%.6f,%d,%d,%d\n",
			id, name, address, lat, lng, total, free, wait)
		file.WriteString(line)
		count++
	}

	fmt.Printf("✅ 导出完成: %s\n", absPath)
	fmt.Printf("📊 共导出 %d 条记录\n", count)
	return nil
}

// 全局配置（在 main 函数中从文件加载）
var globalConfig *Config

// 保存配置到文件
func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %v", err)
	}

	err = os.WriteFile(configFile, data, 0644)
	if err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}

	return nil
}

// 从文件加载配置
func loadConfig() (*Config, error) {
	// 默认配置
	defaultConfig := &Config{
		Authorization: "",
		Interval:      200 * time.Millisecond,
		Duration:      30 * time.Minute,
		MaxBlocks:     5000,
		OutputDB:      "lemobar_scan.db",
		OutputExcel:   "lemobar_export.csv",
	}

	// 如果配置文件不存在，返回默认配置
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		return defaultConfig, nil
	}

	// 读取配置文件
	data, err := os.ReadFile(configFile)
	if err != nil {
		return defaultConfig, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return defaultConfig, fmt.Errorf("解析配置文件失败: %v", err)
	}

	// 验证配置的有效性，如果无效则使用默认值
	if config.Interval <= 0 {
		config.Interval = defaultConfig.Interval
	}
	if config.Duration <= 0 {
		config.Duration = defaultConfig.Duration
	}
	if config.MaxBlocks <= 0 {
		config.MaxBlocks = defaultConfig.MaxBlocks
	}
	if config.OutputDB == "" {
		config.OutputDB = defaultConfig.OutputDB
	}
	if config.OutputExcel == "" {
		config.OutputExcel = defaultConfig.OutputExcel
	}

	return &config, nil
}

// 显示主菜单
func showMainMenu() {
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("           柠檬吧数据爬虫工具 v2.0")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println("1. 开始数据采集")
	fmt.Println("2. 导出数据到CSV")
	fmt.Println("3. 查看当前配置")
	fmt.Println("4. 修改配置")
	fmt.Println("5. 查看数据库统计")
	fmt.Println("6. 帮助说明")
	fmt.Println("0. 退出程序")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Print("请选择操作 (0-6): ")
}

// 显示配置菜单
func showConfigMenu() {
	fmt.Println("\n" + strings.Repeat("-", 40))
	fmt.Println("           配置设置")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println("1. 设置 Authorization 头值")
	fmt.Println("2. 设置请求间隔时间")
	fmt.Println("3. 设置采集时长")
	fmt.Println("4. 设置每城市最大采集数")
	fmt.Println("5. 设置数据库文件路径")
	fmt.Println("6. 设置导出文件路径")
	fmt.Println("0. 返回主菜单")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Print("请选择要修改的配置 (0-6): ")
}

// 读取用户输入
func readInput() string {
	reader := bufio.NewReader(os.Stdin)
	for {
		input, err := reader.ReadString('\n')
		if err != nil {
			// 处理EOF或其他错误
			return ""
		}
		input = strings.TrimSpace(input)
		if input != "" {
			return input
		}
		// 如果输入为空，继续等待
	}
}

// 显示当前配置
func showCurrentConfig() {
	fmt.Println("\n" + strings.Repeat("-", 40))
	fmt.Println("           当前配置")
	fmt.Println(strings.Repeat("-", 40))

	authDisplay := "未设置"
	if globalConfig.Authorization != "" {
		if len(globalConfig.Authorization) > 20 {
			authDisplay = globalConfig.Authorization[:20] + "..."
		} else {
			authDisplay = globalConfig.Authorization
		}
	}

	fmt.Printf("Authorization: %s\n", authDisplay)
	fmt.Printf("请求间隔: %v\n", globalConfig.Interval)
	fmt.Printf("采集时长: %v\n", globalConfig.Duration)
	fmt.Printf("最大采集数/城市: %d\n", globalConfig.MaxBlocks)
	fmt.Printf("数据库文件: %s\n", globalConfig.OutputDB)
	fmt.Printf("导出文件: %s\n", globalConfig.OutputExcel)
	fmt.Println(strings.Repeat("-", 40))
}

// 修改配置
func modifyConfig() {
	for {
		showConfigMenu()
		choice := readInput()

		switch choice {
		case "1":
			fmt.Print("请输入新的 Authorization 头值: ")
			auth := readInput()
			if auth != "" {
				globalConfig.Authorization = auth
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Println("✅ Authorization 已更新并保存")
				}
			} else {
				fmt.Println("❌ Authorization 不能为空")
			}

		case "2":
			fmt.Print("请输入请求间隔时间 (如: 200ms, 1s): ")
			intervalStr := readInput()
			if duration, err := time.ParseDuration(intervalStr); err == nil {
				globalConfig.Interval = duration
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Printf("✅ 请求间隔已更新并保存为: %v\n", duration)
				}
			} else {
				fmt.Println("❌ 时间格式错误，请使用如 200ms, 1s 等格式")
			}

		case "3":
			fmt.Print("请输入采集时长 (如: 30m, 1h): ")
			durationStr := readInput()
			if duration, err := time.ParseDuration(durationStr); err == nil {
				globalConfig.Duration = duration
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Printf("✅ 采集时长已更新并保存为: %v\n", duration)
				}
			} else {
				fmt.Println("❌ 时间格式错误，请使用如 30m, 1h 等格式")
			}

		case "4":
			fmt.Print("请输入每城市最大采集数: ")
			blocksStr := readInput()
			if blocks, err := strconv.Atoi(blocksStr); err == nil && blocks > 0 {
				globalConfig.MaxBlocks = blocks
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Printf("✅ 最大采集数已更新并保存为: %d\n", blocks)
				}
			} else {
				fmt.Println("❌ 请输入有效的正整数")
			}

		case "5":
			fmt.Print("请输入数据库文件路径: ")
			dbPath := readInput()
			if dbPath != "" {
				globalConfig.OutputDB = dbPath
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Printf("✅ 数据库文件路径已更新并保存为: %s\n", dbPath)
				}
			}

		case "6":
			fmt.Print("请输入导出文件路径: ")
			excelPath := readInput()
			if excelPath != "" {
				globalConfig.OutputExcel = excelPath
				if err := saveConfig(globalConfig); err != nil {
					fmt.Printf("⚠️  保存配置失败: %v\n", err)
				} else {
					fmt.Printf("✅ 导出文件路径已更新并保存为: %s\n", excelPath)
				}
			}

		case "0":
			return

		default:
			fmt.Println("❌ 无效选择，请输入 0-6")
		}

		fmt.Print("\n按回车键继续...")
		readInput()
	}
}

// 查看数据库统计
func showDatabaseStats() {
	fmt.Println("\n" + strings.Repeat("-", 40))
	fmt.Println("           数据库统计")
	fmt.Println(strings.Repeat("-", 40))

	db, err := sql.Open("sqlite3", globalConfig.OutputDB)
	if err != nil {
		fmt.Printf("❌ 无法打开数据库: %v\n", err)
		return
	}
	defer db.Close()

	// 检查表是否存在
	var tableExists int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lemobar_areas'").Scan(&tableExists)
	if err != nil || tableExists == 0 {
		fmt.Println("📊 数据库表不存在，还没有采集数据")
		return
	}

	// 统计总记录数
	var totalCount int
	err = db.QueryRow("SELECT COUNT(*) FROM lemobar_areas").Scan(&totalCount)
	if err != nil {
		fmt.Printf("❌ 查询失败: %v\n", err)
		return
	}

	// 统计城市分布
	rows, err := db.Query(`
		SELECT 
			SUBSTR(area_name, 1, 2) as city,
			COUNT(*) as count 
		FROM lemobar_areas 
		GROUP BY SUBSTR(area_name, 1, 2) 
		ORDER BY count DESC 
		LIMIT 10
	`)
	if err == nil {
		defer rows.Close()

		fmt.Printf("📊 总记录数: %d\n", totalCount)
		fmt.Println("📍 城市分布 (Top 10):")

		for rows.Next() {
			var city string
			var count int
			if err := rows.Scan(&city, &count); err == nil {
				fmt.Printf("   %s: %d 条记录\n", city, count)
			}
		}
	} else {
		fmt.Printf("📊 总记录数: %d\n", totalCount)
	}

	fmt.Println(strings.Repeat("-", 40))
}

// 显示帮助信息
func showHelp() {
	fmt.Println("\n" + strings.Repeat("-", 50))
	fmt.Println("                   帮助说明")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Println("📖 使用说明:")
	fmt.Println("1. 首次使用需要设置 Authorization 头值")
	fmt.Println("2. 可以通过 '修改配置' 调整采集参数")
	fmt.Println("3. 数据采集支持多城市并发，自动去重")
	fmt.Println("4. 采集完成后可导出为 CSV 格式")
	fmt.Println()
	fmt.Println("🔧 参数说明:")
	fmt.Println("• Authorization: 从柠檬吧小程序获取的认证令牌")
	fmt.Println("• 请求间隔: 两次API请求之间的等待时间")
	fmt.Println("• 采集时长: 每个城市的最大采集时间")
	fmt.Println("• 最大采集数: 每个城市最多采集的位置点数量")
	fmt.Println()
	fmt.Println("⚠️  注意事项:")
	fmt.Println("• 请勿设置过短的请求间隔，避免被限制")
	fmt.Println("• Authorization 令牌可能会过期，需要重新获取")
	fmt.Println("• 数据库文件较大时，导出可能需要一些时间")
	fmt.Println()
	fmt.Println("🏙️  支持城市:")
	fmt.Println("北京、上海、广州、深圳、杭州、南京、成都、重庆")
	fmt.Println("武汉、西安、天津、苏州、郑州、长沙、青岛、宁波")
	fmt.Println("佛山、合肥、无锡、厦门、大连、南昌、昆明、常州")
	fmt.Println(strings.Repeat("-", 50))
}

// 开始数据采集
func startDataCollection() {
	fmt.Println("\n🚀 准备开始数据采集...")

	// 检查必需配置
	if globalConfig.Authorization == "" {
		fmt.Println("❌ 错误: 必须先设置 Authorization 头值")
		fmt.Println("请选择 '4. 修改配置' 来设置 Authorization")
		return
	}

	// 显示配置信息
	fmt.Println("\n📋 当前采集配置:")
	showCurrentConfig()

	fmt.Print("确认开始采集吗? (y/N): ")
	confirm := readInput()
	if strings.ToLower(confirm) != "y" && strings.ToLower(confirm) != "yes" {
		fmt.Println("❌ 采集已取消")
		return
	}

	// 初始化数据库
	db, err := sql.Open("sqlite3", globalConfig.OutputDB)
	if err != nil {
		fmt.Printf("❌ 打开数据库失败: %v\n", err)
		return
	}
	defer db.Close()

	// 创建表
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS lemobar_areas (
		area_id INTEGER PRIMARY KEY,
		area_name TEXT,
		detail_address TEXT,
		latitude REAL,
		longitude REAL,
		total_device_num INTEGER,
		free_device_num INTEGER,
		wait_duration INTEGER
	)`)

	// 城市列表
	centers := []Center{
		{"北京", 39.9042, 116.4074}, {"上海", 31.2304, 121.4737}, {"广州", 23.1291, 113.2644},
		{"深圳", 22.5431, 114.0579}, {"杭州", 30.2741, 120.1551}, {"南京", 32.0603, 118.7969},
		{"成都", 30.5728, 104.0668}, {"重庆", 29.5630, 106.5516}, {"武汉", 30.5928, 114.3055},
		{"西安", 34.3416, 108.9398}, {"天津", 39.3434, 117.3616}, {"苏州", 31.2989, 120.5853},
		{"郑州", 34.7466, 113.6254}, {"长沙", 28.2282, 112.9388}, {"青岛", 36.0671, 120.3826},
		{"宁波", 29.8683, 121.5440}, {"佛山", 23.0215, 113.1214}, {"合肥", 31.8206, 117.2272},
		{"无锡", 31.4912, 120.3119}, {"厦门", 24.4798, 118.0894}, {"大连", 38.9140, 121.6147},
		{"南昌", 28.6829, 115.8582}, {"昆明", 25.0389, 102.7183}, {"常州", 31.8107, 119.9741},
	}

	fmt.Printf("\n🎯 开始采集 %d 个城市的数据...\n", len(centers))
	fmt.Println("💡 采集过程中按 Ctrl+C 可以中止")

	// 开始采集
	startTime := time.Now()
	var wg sync.WaitGroup
	for _, c := range centers {
		wg.Add(1)
		go spiralScan(db, c, globalConfig, &wg)
	}
	wg.Wait()

	fmt.Printf("\n✅ 所有城市扫描任务已完成，总耗时: %.2f 分钟\n", time.Since(startTime).Minutes())

	// 询问是否自动导出
	fmt.Print("是否立即导出数据到CSV? (Y/n): ")
	exportChoice := readInput()
	if strings.ToLower(exportChoice) != "n" && strings.ToLower(exportChoice) != "no" {
		if err := exportToExcel(globalConfig); err != nil {
			fmt.Printf("❌ 自动导出失败: %v\n", err)
		}
	}
}

func main() {
	fmt.Println("欢迎使用柠檬吧数据爬虫工具!")

	// 加载配置
	var err error
	globalConfig, err = loadConfig()
	if err != nil {
		fmt.Printf("⚠️  加载配置文件失败，使用默认配置: %v\n", err)
		// 即使加载失败，loadConfig 也会返回默认配置
	} else {
		fmt.Println("✅ 配置已从文件加载")
	}

	// 主程序循环
	for {
		showMainMenu()
		choice := readInput()

		switch choice {
		case "1":
			startDataCollection()

		case "2":
			fmt.Println("\n🔄 开始导出数据...")
			if err := exportToExcel(globalConfig); err != nil {
				fmt.Printf("❌ 导出失败: %v\n", err)
			}

		case "3":
			showCurrentConfig()

		case "4":
			modifyConfig()

		case "5":
			showDatabaseStats()

		case "6":
			showHelp()

		case "0":
			fmt.Println("\n👋 感谢使用柠檬吧数据爬虫工具!")
			fmt.Println("再见!")
			os.Exit(0)

		default:
			fmt.Println("❌ 无效选择，请输入 0-6")
		}

		// 等待用户按回车继续
		if choice != "4" { // 配置菜单有自己的等待逻辑
			fmt.Print("\n按回车键继续...")
			readInput()
		}
	}
}
