package install_go

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"gopkg.in/cheggaaa/pb.v1"
)

var (
	DefaultDownloadURLPrefix = "https://dl.google.com/go"
	DefaultProxyURL          = ""
	DefaultSourceURL         = "https://go.googlesource.com/go/+refs?format=TEXT"
)

// https://dl.google.com/go
func downloadGoVersion(target, dest string) error {
	if DefaultProxyURL != "" {
		os.Setenv("HTTPS_PROXY", DefaultProxyURL)
	}

	uri := fmt.Sprintf("%s/%s", DefaultDownloadURLPrefix, target)

	fmt.Printf("开始下载 Go 安装包: %s\n", uri)

	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return err
	}
	req.Header.Add("User-Agent", fmt.Sprintf("golang.org-getgo/%s", target))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载 Go 安装包失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return fmt.Errorf("下载 Go 安装包失败: HTTP %d: %s", resp.StatusCode, uri)
	}

	size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
	if err != nil {
		return err
	}

	cachePath := filepath.Join("cache", "downloads")
	os.MkdirAll(cachePath, os.ModePerm)
	targetName := filepath.Join(cachePath, target)
	os.Remove(targetName)
	targetFile, err := os.OpenFile(targetName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	if err != nil {
		return err
	}
	defer targetFile.Close()

	bar := pb.New(size).SetUnits(pb.U_BYTES)
	bar.Start()

	h := sha256.New()
	w := io.MultiWriter(targetFile, h, bar)
	if _, err := io.Copy(w, resp.Body); err != nil {
		bar.Finish()
		return err
	}

	bar.Finish()

	sresp, err := http.Get(uri + ".sha256")
	if err != nil {
		return fmt.Errorf("获取文件 %s 失败: %v", uri, err)
	}
	defer sresp.Body.Close()

	if sresp.StatusCode > 299 {
		return fmt.Errorf("获取 %s 失败: %d", uri, sresp.StatusCode)
	}

	shasum, err := ioutil.ReadAll(sresp.Body)
	if err != nil {
		return err
	}

	sum := fmt.Sprintf("%x", h.Sum(nil))
	if sum != string(shasum) {
		return fmt.Errorf("下载的文件 HASH 与服务器的文件 HASH 不匹配: %s != %s", sum, string(shasum))
	}

	unpackFn := unpackTar
	if runtime.GOOS == "windows" {
		unpackFn = unpackZip
	}

	os.RemoveAll(dest)
	fmt.Println("正在解压 Go 安装包...")
	if err := unpackFn(targetFile.Name(), dest); err != nil {
		return fmt.Errorf("解压 Go 到目标路径 %s 失败: %v", dest, err)
	}

	return nil
}

func unpack(dest, name string, fi os.FileInfo, r io.Reader) error {
	if strings.HasPrefix(name, "go/") {
		name = name[len("go/"):]
	}

	path := filepath.Join(dest, name)
	if fi.IsDir() {
		return os.MkdirAll(path, fi.Mode())
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return err
	}
	defer f.Close()

	bar := pb.New64(fi.Size()).SetUnits(pb.U_BYTES)
	bar.Prefix(name)
	bar.Start()

	w := io.MultiWriter(f, bar)

	_, err = io.Copy(w, r)

	bar.Finish()
	return err
}

func unpackTar(src, dest string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	archive, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer archive.Close()

	tarReader := tar.NewReader(archive)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		if err := unpack(dest, header.Name, header.FileInfo(), tarReader); err != nil {
			return err
		}
	}

	return nil
}

func unpackZip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}

	for _, f := range zr.File {
		fr, err := f.Open()
		if err != nil {
			return err
		}
		if err := unpack(dest, f.Name, f.FileInfo(), fr); err != nil {
			return err
		}
		fr.Close()
	}

	return nil
}

func getAllGoVersion() ([]string, error) {
	getRemoteVersion := func(name string) ([]byte, error) {
		if DefaultProxyURL != "" {
			os.Setenv("HTTPS_PROXY", DefaultProxyURL)
		}

		resp, err := http.Get(DefaultSourceURL)
		if err != nil {
			return nil, fmt.Errorf("无法连接到 Go 版本号获取地址: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode > 299 {
			return nil, fmt.Errorf("无法获取到 Go 版本号列表: %d", resp.StatusCode)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		if err = ioutil.WriteFile(name, b, os.ModePerm); err != nil {
			return nil, fmt.Errorf("缓存版本文件错误: %v", err)
		}

		return b, nil
	}

	var (
		b   []byte
		err error
	)

	versionName := filepath.Join("cache", "VERSION")
	if fi, e := os.Stat(versionName); os.IsNotExist(e) {
		b, err = getRemoteVersion(versionName)
		if err != nil {
			return nil, err
		}
	} else {
		if time.Now().Sub(fi.ModTime()) < time.Hour*72 {
			b, err = ioutil.ReadFile(versionName)
			if err != nil {
				return nil, fmt.Errorf("读取版本缓存文件错误: %v", err)
			}
		} else {
			b, err = getRemoteVersion(versionName)
			if err != nil {
				return nil, err
			}
		}
	}

	sortVersions := make([]string, 0)
	tmp := make(map[string]string)

	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "refs/tags/go") {
			ls := strings.Fields(line)
			if len(ls) == 2 {
				version := strings.Replace(ls[1], "refs/tags/go", "", -1)
				oldVersion := version
				sections := strings.Split(version, ".")
				switch len(sections) {
				case 1:
					version += ".0.0"
				case 2:
					if strings.Contains(version, "beta") {
						version = strings.Replace(version, "beta", ".0-beta", -1)
					} else if strings.Contains(version, "rc") {
						version = strings.Replace(version, "rc", ".0-rc", -1)
					} else {
						version += ".0"
					}
				case 3:
					if strings.Contains(version, "rc") {
						version = strings.Replace(version, "rc", "-rc", -1)
					} else if strings.Contains(version, "beta") {
						version = strings.Replace(version, "beta", "-beta", -1)
					}
				}

				tmp[version] = oldVersion
				sortVersions = append(sortVersions, version)
			}
		}
	}

	sort.SliceStable(sortVersions, func(i, j int) bool {
		v1, _ := semver.Make(sortVersions[i])
		v2, _ := semver.Make(sortVersions[j])
		return v1.GE(v2)
	})

	versions := make([]string, 0)
	for _, value := range sortVersions {
		versions = append(versions, tmp[value])
	}

	return versions, nil
}
