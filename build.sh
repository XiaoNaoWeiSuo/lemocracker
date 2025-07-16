#!/bin/bash
# 构建脚本 - 生成跨平台可执行文件

echo "开始构建柠檬吧爬虫工具..."

# 创建构建目录
mkdir -p build

# 设置 CGO 环境变量，因为 go-sqlite3 需要 CGO 支持
export CGO_ENABLED=1

# 构建Windows 64位版本
echo "构建 Windows 64位版本..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go build -o build/lemobar-crawler-windows-amd64.exe main.go

# 构建Linux 64位版本  
echo "构建 Linux 64位版本..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o build/lemobar-crawler-linux-amd64 main.go

# 构建macOS 64位版本
echo "构建 macOS 64位版本..."
GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o build/lemobar-crawler-macos-amd64 main.go

# 构建macOS ARM64版本 (Apple Silicon)
echo "构建 macOS ARM64版本..."
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -o build/lemobar-crawler-macos-arm64 main.go

echo "构建完成! 可执行文件位于 build/ 目录中"
echo ""
echo "文件列表:"
ls -la build/

echo ""
echo "使用方法:"
echo "直接运行程序即可进入交互式界面："
echo "Windows: build/lemobar-crawler-windows-amd64.exe"
echo "Linux:   build/lemobar-crawler-linux-amd64"
echo "macOS:   build/lemobar-crawler-macos-amd64"

echo ""
echo "注意:"
echo "- Windows版本需要在安装了mingw-w64的环境中编译"
echo "- 如果跨平台编译失败，请在目标平台直接编译:"
echo "  go build -o lemobar-crawler main.go"
