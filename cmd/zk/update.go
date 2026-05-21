package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mote/internal/shared"
)

const githubReleasesAPI = "https://api.github.com/repos/merlin-node/mote/releases/latest"

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func cmdUpdate() {
	fmt.Println("🔍 正在检查最新版本...")

	rel, err := fetchLatestRelease()
	if err != nil {
		fmt.Printf("❌ 获取版本信息失败: %v\n", err)
		return
	}

	latest := rel.TagName
	current := shared.Version
	fmt.Printf("   当前版本: %s\n", current)
	fmt.Printf("   最新版本: %s\n", latest)

	if normalizeVersion(latest) == normalizeVersion(current) {
		fmt.Println("✅ 已是最新版本，无需更新")
		return
	}

	arch := runtime.GOARCH // amd64 / arm64
	assetName := fmt.Sprintf("zk-linux-%s", arch)
	var asset *ghAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		fmt.Printf("❌ 找不到适合当前平台的二进制: %s\n", assetName)
		fmt.Println("   请到 GitHub Releases 手动下载")
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("❌ 无法获取当前二进制路径: %v\n", err)
		return
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		fmt.Printf("❌ 无法解析路径: %v\n", err)
		return
	}

	fmt.Printf("📥 正在下载 %s (%s)...\n", assetName, rel.TagName)
	tmpPath := exePath + ".new"
	checksum, err := downloadFile(asset.BrowserDownloadURL, tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		fmt.Printf("❌ 下载失败: %v\n", err)
		return
	}
	fmt.Printf("   SHA256: %s\n", checksum)

	// 备份旧二进制
	backupPath := exePath + ".bak"
	if err := copyFile(exePath, backupPath); err != nil {
		os.Remove(tmpPath)
		fmt.Printf("❌ 备份旧版本失败: %v\n", err)
		return
	}
	fmt.Printf("   旧版本已备份至 %s\n", backupPath)

	// 设置可执行权限
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		fmt.Printf("❌ 设置执行权限失败: %v\n", err)
		return
	}

	// 原子替换
	if err := os.Rename(tmpPath, exePath); err != nil {
		os.Remove(tmpPath)
		fmt.Printf("❌ 替换二进制失败: %v\n", err)
		return
	}
	fmt.Println("✅ 二进制已替换，正在重启服务...")

	// 重启服务
	cmdSystemctl("restart", "zk")
	fmt.Println("   等待服务启动...")
	time.Sleep(3 * time.Second)

	// 健康检查
	cfg, _ := loadConfigForUpdate()
	if healthCheck(cfg) {
		fmt.Printf("✅ 更新成功！运行版本: %s\n", latest)
		os.Remove(backupPath) // 健康则删备份
	} else {
		fmt.Println("❌ 健康检查失败，正在回滚...")
		if err := copyFile(backupPath, exePath); err != nil {
			fmt.Printf("   回滚失败（请手动恢复 %s → %s）: %v\n", backupPath, exePath, err)
		} else {
			os.Chmod(exePath, 0755)
			cmdSystemctl("restart", "zk")
			fmt.Printf("✅ 已回滚至 %s\n", current)
		}
	}
}

func fetchLatestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, githubReleasesAPI, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "mote-updater/"+shared.Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadFile(url, destPath string) (checksum string, err error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func healthCheck(cfg *serverCfg) bool {
	addr := "http://127.0.0.1" + cfg.listen
	client := &http.Client{Timeout: 3 * time.Second}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, addr+"/api/config", nil)
		req.SetBasicAuth(cfg.user, cfg.pass)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode/100 == 2 {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

type serverCfg struct {
	listen string
	user   string
	pass   string
}

func loadConfigForUpdate() (*serverCfg, error) {
	type minCfg struct {
		Listen        string `json:"listen"`
		AdminUsername string `json:"admin_username"`
		AdminPassword string `json:"admin_password"`
	}
	data, err := os.ReadFile("/etc/zk/config.json")
	if err != nil {
		return &serverCfg{listen: ":1888"}, err
	}
	var c minCfg
	json.Unmarshal(data, &c)
	if c.Listen == "" {
		c.Listen = ":1888"
	}
	if strings.HasPrefix(c.Listen, ":") {
		c.Listen = "127.0.0.1" + c.Listen
	}
	return &serverCfg{listen: c.Listen, user: c.AdminUsername, pass: c.AdminPassword}, nil
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
