package asset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/getsavvyinc/upgrade-cli/release"
)

type cleanupFn func() error

type Downloader interface {
	DownloadAsset(ctx context.Context, ReleaseAssets []release.Asset) (*Info, cleanupFn, error)
}

type Info struct {
	Checksum                 string
	DownloadedBinaryFilePath string
	PlatformSuffix           string
	ArSuffix                 string
}

type downloader struct {
	os             string
	arch           string
	executablePath string
}

var _ Downloader = (*downloader)(nil)

type AssetDownloadOpt func(*downloader)

func WithOS(os string) AssetDownloadOpt {
	return func(d *downloader) {
		d.os = os
	}
}

func WithArch(arch string) AssetDownloadOpt {
	return func(d *downloader) {
		d.arch = arch
	}
}

func NewAssetDownloader(executablePath string, opts ...AssetDownloadOpt) Downloader {
	d := &downloader{
		os:             runtime.GOOS,
		arch:           runtime.GOARCH,
		executablePath: executablePath,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

var ErrNoAsset = errors.New("no asset found")

func (d *downloader) DownloadAsset(ctx context.Context, assets []release.Asset) (*Info, cleanupFn, error) {
	// iterate through the assets and find the one that matches the os and arch
	suffix := d.os + "_" + d.arch
	for _, asset := range assets {
		u := strings.ToLower(asset.BrowserDownloadURL)
		// Remove .tar.gz .tar .zip .gz from the end of the string
		// and compare the suffix
		// e.g. linux_amd64.tar.gz -> linux_amd64
		var ar string
		for _, s := range []string{".tar.gz", ".tar", ".zip", ".gz"} {
			t := strings.TrimSuffix(u, s)
			if t != u {
				ar = s
				u = t
				break
			}
		}

		if strings.HasSuffix(u, suffix) {
			info, c, err := d.downloadAsset(ctx, asset.BrowserDownloadURL)
			if err != nil {
				return nil, nil, err
			}

			info.PlatformSuffix = suffix
			info.ArSuffix = ar

			return info, c, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: os:%s arch:%s", ErrNoAsset, d.os, d.arch)
}

func (d *downloader) downloadAsset(ctx context.Context, url string) (*Info, cleanupFn, error) {
	executable := filepath.Base(d.executablePath)

	// Download the file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	// Create a temporary file
	tmpFile, err := os.CreateTemp("", executable)
	if err != nil {
		return nil, nil, err
	}
	defer tmpFile.Close()

	cleanupFn := func() error {
		return os.Remove(tmpFile.Name())
	}

	// sha256 checksum
	hasher := sha256.New()

	// Write the response body to the temporary file and hasher
	rd := io.TeeReader(resp.Body, hasher)
	_, err = io.Copy(tmpFile, rd)
	if err != nil {
		cleanupFn()
		return nil, nil, err
	}

	// Ensure the downloaded file has executable permissions
	if err := os.Chmod(tmpFile.Name(), 0755); err != nil {
		cleanupFn()
		return nil, nil, err
	}

	return &Info{
		Checksum:                 hex.EncodeToString(hasher.Sum(nil)),
		DownloadedBinaryFilePath: tmpFile.Name(),
	}, cleanupFn, nil
}
