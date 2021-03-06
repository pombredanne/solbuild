//
// Copyright © 2016-2017 Ikey Doherty <ikey@solus-project.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package source

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	log "github.com/Sirupsen/logrus"
	curl "github.com/andelf/go-curl"
	"github.com/cheggaaa/pb"
	"github.com/jlaffaye/ftp"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A SimpleSource is a tarball or other source for a package
type SimpleSource struct {
	URI  string
	File string // Basename of the file

	legacy    bool   // If this is ypkg or not
	validator string // Validation key for this source

	url *url.URL
}

// NewSimple will create a new source instance
func NewSimple(uri, validator string, legacy bool) (*SimpleSource, error) {
	// Ensure the URI is actually valid.
	uriObj, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	ret := &SimpleSource{
		URI:       uri,
		File:      filepath.Base(uriObj.Path),
		legacy:    legacy,
		validator: validator,
		url:       uriObj,
	}
	return ret, nil
}

// GetIdentifier will return the URI associated with this source.
func (s *SimpleSource) GetIdentifier() string {
	return s.URI
}

// GetBindConfiguration will return the pair for binding our tarballs.
func (s *SimpleSource) GetBindConfiguration(rootfs string) BindConfiguration {
	return BindConfiguration{
		BindSource: s.GetPath(s.validator),
		BindTarget: filepath.Join(rootfs, s.File),
	}
}

// GetPath gets the path on the filesystem of the source
func (s *SimpleSource) GetPath(hash string) string {
	return filepath.Join(SourceDir, hash, s.File)
}

// GetSHA1Sum will return the sha1sum for the given path
func (s *SimpleSource) GetSHA1Sum(path string) (string, error) {
	inp, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha1.New()
	hash.Write(inp)
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum), nil
}

// GetSHA256Sum will return the sha1sum for the given path
func (s *SimpleSource) GetSHA256Sum(path string) (string, error) {
	inp, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write(inp)
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum), nil
}

// IsFetched will determine if the source is already present
func (s *SimpleSource) IsFetched() bool {
	return PathExists(s.GetPath(s.validator))
}

// download will proxy the download to the correct scheme handler
func (s *SimpleSource) download(destination string) error {
	// Fix up the http client
	switch s.url.Scheme {
	case "ftp":
		return s.downloadFTP(destination)
	default:
		return s.downloadCurl(destination)
	}
}

// downloadCURL utilises CURL to do all downloads
func (s *SimpleSource) downloadCurl(destination string) error {
	hnd := curl.EasyInit()
	defer hnd.Cleanup()

	hnd.Setopt(curl.OPT_URL, s.URI)
	hnd.Setopt(curl.OPT_FOLLOWLOCATION, 1)

	out, err := os.Create(destination)
	if err != nil {
		return err
	}

	pbar := pb.New64(0).Prefix(filepath.Base(destination))
	pbar.Set(0)
	pbar.SetUnits(pb.U_BYTES)
	pbar.SetMaxWidth(80)
	pbar.ShowSpeed = true

	writer := func(data []byte, udata interface{}) bool {
		if _, err := out.Write(data); err != nil {
			return false
		}
		return true
	}
	progress := func(total, now, utotal, unow float64, udata interface{}) bool {
		pbar.Total = int64(total)
		pbar.Set64(int64(now))
		pbar.Update()
		return true
	}

	hnd.Setopt(curl.OPT_WRITEFUNCTION, writer)
	hnd.Setopt(curl.OPT_NOPROGRESS, false)
	hnd.Setopt(curl.OPT_PROGRESSFUNCTION, progress)
	// Enforce internal 300 second connect timeout in libcurl
	hnd.Setopt(curl.OPT_CONNECTTIMEOUT, 0)
	hnd.Setopt(curl.OPT_USERAGENT, fmt.Sprintf("solbuild 1.3.0"))

	pbar.Start()
	defer func() {
		pbar.Update()
		pbar.Finish()
	}()

	return hnd.Perform()
}

// downloadFTP will fetch a file over ftp using anonymous credentials
func (s *SimpleSource) downloadFTP(destination string) error {
	hostAddr := s.url.Host
	// Assign a port if not set
	if !strings.Contains(hostAddr, ":") {
		hostAddr += ":21"
	}
	client, err := ftp.DialTimeout(hostAddr, time.Minute*2)
	if err != nil {
		return err
	}
	defer client.Quit()

	// Get the relevant credentials
	username := "anonymous"
	password := "anonymous"
	if s.url.User != nil {
		username = s.url.User.Username()
		if pwd, set := s.url.User.Password(); set {
			password = pwd
		} else {
			password = ""
		}
	}

	// Login to the server
	log.WithFields(log.Fields{
		"username": username,
	}).Info("Logging into FTP server")
	if err := client.Login(username, password); err != nil {
		return err
	}

	// Try to list the file
	toFetch := s.url.Path
	log.WithFields(log.Fields{
		"path": toFetch,
	}).Info("Getting remote file information")
	entries, err := client.List(toFetch)
	if err != nil {
		return err
	}

	// Assert *1* file
	if len(entries) != 1 {
		return fmt.Errorf("FTP expected 1 file, found %d files", len(entries))
	}

	// Try to RETR the file
	fileLen := entries[0].Size
	resp, err := client.Retr(toFetch)
	if err != nil {
		return err
	}
	defer resp.Close()

	// Set the output
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()

	// Set up the progressbar & hooks
	pbar := pb.New64(int64(fileLen)).Prefix(filepath.Base(destination))
	pbar.SetUnits(pb.U_BYTES)
	pbar.SetMaxWidth(80)
	pbar.ShowSpeed = true
	reader := pbar.NewProxyReader(resp)
	pbar.Start()
	defer func() {
		pbar.Update()
		pbar.Finish()
	}()

	// Now actually download it
	if _, err := io.Copy(out, reader); err != nil {
		return err
	}
	return nil
}

// Fetch will download the given source and cache it locally
func (s *SimpleSource) Fetch() error {
	// Now go and download it
	log.WithFields(log.Fields{
		"uri": s.URI,
	}).Debug("Downloading source")

	destPath := filepath.Join(SourceStagingDir, s.File)

	// Check staging is available
	if !PathExists(SourceStagingDir) {
		if err := os.MkdirAll(SourceStagingDir, 00755); err != nil {
			return err
		}
	}

	// Grab the file
	if err := s.download(destPath); err != nil {
		return err
	}

	hash, err := s.GetSHA256Sum(destPath)
	if err != nil {
		return err
	}

	// Make the target directory
	tgtDir := filepath.Join(SourceDir, hash)
	if !PathExists(tgtDir) {
		if err := os.MkdirAll(tgtDir, 00755); err != nil {
			return err
		}
	}
	// Move from staging into hash based directory
	dest := filepath.Join(tgtDir, s.File)
	if err := os.Rename(destPath, dest); err != nil {
		return err
	}
	// If the file has a sha1sum set, symlink it to the sha256sum because
	// it's a legacy archive (pspec.xml)
	if s.legacy {
		sha, err := s.GetSHA1Sum(dest)
		if err != nil {
			return err
		}
		tgtLink := filepath.Join(SourceDir, sha)
		if err := os.Symlink(hash, tgtLink); err != nil {
			return err
		}
	}
	return nil
}
