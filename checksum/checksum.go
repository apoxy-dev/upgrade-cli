package checksum

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/getsavvyinc/upgrade-cli/release"
)

type Downloader interface {
	Download(ctx context.Context, assets []release.Asset) (*Info, error)
}

type Info struct {
	// keyed on $binary_os_$arch
	Checksums map[string]string
}

type checksumDownloader struct {
	assetSuffix string
}

type DownloadOpt func(*checksumDownloader)

func WithAssetSuffix(suffix string) DownloadOpt {
	return func(c *checksumDownloader) {
		c.assetSuffix = suffix
	}
}

func NewCheckSumDownloader(opts ...DownloadOpt) Downloader {
	d := &checksumDownloader{
		assetSuffix: "checksums.txt",
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

var ErrNoCheckSumAsset = errors.New("no checksum asset found")

func (c *checksumDownloader) Download(ctx context.Context, assets []release.Asset) (*Info, error) {
	// iterate through the assets and find the one that matches the os and arch
	for _, asset := range assets {
		if strings.HasSuffix(asset.BrowserDownloadURL, c.assetSuffix) {
			checksums, err := downloadCheckSum(ctx, asset.BrowserDownloadURL)
			if err != nil {
				return nil, err
			}
			return checksums, nil
		}
	}
	return nil, ErrNoCheckSumAsset
}

var ErrInvalidChecksumFile = errors.New("invalid checksum file")

func downloadCheckSum(ctx context.Context, url string) (*Info, error) {
	// download the checksum file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	checksums := make(map[string]string)

	scanner := bufio.NewScanner(resp.Body)
	// parse the file and return the checksums
	for scanner.Scan() {
		line := scanner.Text()
		// parse the line and extract the checksum
		line = strings.TrimSpace(line)
		// there maybe one or more blank spaces between the checksum and the file name
		parts := strings.Fields(line)
		// parts[0] is the checksum, parts[1] is the file name
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: checksum file is malformed", ErrInvalidChecksumFile)
		}
		k := strings.ToLower(parts[1])
		for _, s := range []string{".tar.gz", ".tar", ".zip", ".gz"} {
			k = strings.TrimSuffix(k, s)
		}
		checksums[k] = strings.ToLower(parts[0])
	}

	if len(checksums) == 0 {
		return nil, fmt.Errorf("%w: checksum file is empty", ErrInvalidChecksumFile)
	}
	return &Info{Checksums: checksums}, nil
}

type CheckSumValidator interface {
	IsCheckSumValid(ctx context.Context, binary string, checksums *Info, downloadedChecksum string) bool
}

type validator struct {
	os   string
	arch string
}

type ValidatorOption func(*validator)

func WithOS(os string) ValidatorOption {
	return func(v *validator) {
		v.os = os
	}
}

func WithArch(arch string) ValidatorOption {
	return func(v *validator) {
		v.arch = arch
	}
}

func NewCheckSumValidator(opts ...ValidatorOption) CheckSumValidator {
	v := &validator{
		os:   runtime.GOOS,
		arch: runtime.GOARCH,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (v *validator) IsCheckSumValid(ctx context.Context, binary string, info *Info, downloadedChecksum string) bool {

	key := fmt.Sprintf("%s_%s_%s", binary, v.os, v.arch)
	expectedChecksum, ok := info.Checksums[key]
	if !ok {
		return false
	}
	return expectedChecksum == downloadedChecksum
}
