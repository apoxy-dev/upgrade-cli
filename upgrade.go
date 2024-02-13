package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/getsavvyinc/upgrade-cli/checksum"
	"github.com/getsavvyinc/upgrade-cli/release"
	"github.com/getsavvyinc/upgrade-cli/release/asset"
	"github.com/hashicorp/go-version"
)

type Upgrader interface {
	IsNewVersionAvailable(ctx context.Context, currentVersion string) (bool, error)
	// Upgrade upgrades the current binary to the latest version.
	Upgrade(ctx context.Context, currentVersion string) error
}

type upgrader struct {
	executablePath     string
	repo               string
	owner              string
	releaseGetter      release.Getter
	assetDownloader    asset.Downloader
	checksumDownloader checksum.Downloader
	checksumValidator  checksum.CheckSumValidator
}

var _ Upgrader = (*upgrader)(nil)

type Opt func(*upgrader)

func WithAssetDownloader(d asset.Downloader) Opt {
	return func(u *upgrader) {
		u.assetDownloader = d
	}
}

func WithCheckSumDownloader(c checksum.Downloader) Opt {
	return func(u *upgrader) {
		u.checksumDownloader = c
	}
}

func WithCheckSumValidator(c checksum.CheckSumValidator) Opt {
	return func(u *upgrader) {
		u.checksumValidator = c
	}
}

func NewUpgrader(owner string, repo string, executablePath string, opts ...Opt) Upgrader {
	u := &upgrader{
		repo:               repo,
		owner:              owner,
		executablePath:     executablePath,
		releaseGetter:      release.NewReleaseGetter(repo, owner),
		assetDownloader:    asset.NewAssetDownloader(executablePath),
		checksumDownloader: checksum.NewCheckSumDownloader(),
		checksumValidator:  checksum.NewCheckSumValidator(),
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

var ErrInvalidCheckSum = errors.New("invalid checksum")

func (u *upgrader) IsNewVersionAvailable(ctx context.Context, currentVersion string) (bool, error) {
	curr, err := version.NewVersion(currentVersion)
	if err != nil {
		return false, fmt.Errorf("failed to parse current version: %s with err %w", currentVersion, err)
	}

	releaseInfo, err := u.releaseGetter.GetLatestRelease(ctx)
	if err != nil {
		return false, err
	}

	latest, err := version.NewVersion(releaseInfo.TagName)
	if err != nil {
		return false, fmt.Errorf("failed to parse latest version: %s with err %w", releaseInfo.TagName, err)
	}

	return latest.GreaterThan(curr), nil
}

func (u *upgrader) Upgrade(ctx context.Context, currentVersion string) error {
	curr, err := version.NewVersion(currentVersion)
	if err != nil {
		return err
	}

	releaseInfo, err := u.releaseGetter.GetLatestRelease(ctx)
	if err != nil {
		return err
	}

	latest, err := version.NewVersion(releaseInfo.TagName)
	if err != nil {
		return err
	}

	if latest.LessThanOrEqual(curr) {
		return nil
	}

	// from the releaseInfo, download the binary for the architecture

	downloadInfo, cleanup, err := u.assetDownloader.DownloadAsset(ctx, releaseInfo.Assets)
	if err != nil {
		return err
	}

	if cleanup != nil {
		defer cleanup()
	}

	// download the checksum file
	checksumInfo, err := u.checksumDownloader.Download(ctx, releaseInfo.Assets)
	if err != nil {
		return err
	}

	executableName := filepath.Base(u.executablePath)
	// verify the checksum
	if !u.checksumValidator.IsCheckSumValid(ctx, executableName, checksumInfo, downloadInfo.Checksum) {
		return ErrInvalidCheckSum
	}

	tempFile, err := tryUnArchive(executableName, downloadInfo.DownloadedBinaryFilePath)
	if err != nil {
		return fmt.Errorf("failed to unarchive: %w", err)
	}
	defer os.Remove(tempFile)

	if err := replaceBinary(tempFile, u.executablePath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}

// tryUnArchive unarchives the downloaded update and returns the path to the unarchived temp file.
func tryUnArchive(prefix, arPath string) (string, error) {
	f, err := os.Open(arPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	switch filepath.Ext(arPath) {
	case ".tar.gz":
		return unTarGz(prefix, f)
	case ".zip":
		return unZip(prefix, f)
	case ".tar":
		return unTar(prefix, f)
	case ".gz":
		return unGz(prefix, f)
	case "": // no extension - assume it's a binary
		return arPath, nil
	default:
		return "", fmt.Errorf("unsupported file type: %s", filepath.Ext(arPath))
	}
}

// unTarGz unarchives a .tar.gz file.
func unTarGz(prefix string, r io.Reader) (string, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()
	return unTar(prefix, gzr)
}

// unTar unarchives a .tar file.
func unTar(prefix string, r io.Reader) (string, error) {
	tarr := tar.NewReader(r)
	out, err := os.CreateTemp("", "/tmp/"+prefix+"-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer out.Close()

	for {
		hdr, err := tarr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read next header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !strings.HasPrefix(filepath.Base(hdr.Name), prefix) {
			continue
		}

		if _, err := io.Copy(out, tarr); err != nil {
			return "", fmt.Errorf("failed to copy file: %w", err)
		}

		if err := os.Chmod(out.Name(), 0755); err != nil {
			return "", fmt.Errorf("failed to change file permissions: %w", err)
		}

		return out.Name(), nil
	}

	return "", fmt.Errorf("file not found in archive")
}

// unZip unarchives a .zip file.
func unZip(prefix string, r io.ReaderAt) (string, error) {
	zr, err := zip.NewReader(r, 0)
	if err != nil {
		return "", fmt.Errorf("failed to create zip reader: %w", err)
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(filepath.Base(f.Name), prefix) {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open file: %w", err)
		}
		defer rc.Close()
		out, err := os.CreateTemp("", "/tmp/"+prefix+"-*")
		if err != nil {
			return "", fmt.Errorf("failed to create temp file: %w", err)
		}
		defer out.Close()

		if _, err := io.Copy(out, rc); err != nil {
			return "", fmt.Errorf("failed to copy file: %w", err)
		}

		if err := os.Chmod(out.Name(), 0755); err != nil {
			return "", fmt.Errorf("failed to change file permissions: %w", err)
		}

		return out.Name(), nil
	}

	return "", fmt.Errorf("no file found with prefix: %s", prefix)
}

// unGz unarchives a .gz file.
// It returns the path to the unarchived temp file.
func unGz(prefix string, r io.Reader) (string, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	out, err := os.CreateTemp("", "/tmp/"+prefix+"-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, gzr); err != nil {
		return "", fmt.Errorf("failed to copy file: %w", err)
	}

	if err := os.Chmod(out.Name(), 0755); err != nil {
		return "", fmt.Errorf("failed to change file permissions: %w", err)
	}

	return out.Name(), nil
}

// replaceBinary replaces the current executable with the downloaded update.
func replaceBinary(tmpFilePath, currentBinaryPath string) error {
	// Replace the current binary with the new binary
	if err := os.Rename(tmpFilePath, currentBinaryPath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}
