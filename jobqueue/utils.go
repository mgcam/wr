// Copyright © 2016-2018 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package jobqueue

// This file contains some general utility functions for use by client and
// server.

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dgryski/go-farm"
	multierror "github.com/hashicorp/go-multierror"
)

// AppName gets used in certain places like naming the base directory of created
// working directories during Client.Execute().
var AppName = "jobqueue"

// mkHashedLevels is the number of directory levels we create in mkHashedDirs
const mkHashedLevels = 4

var pss = []byte("Pss:")

// cr, lf and ellipses get used by stdFilter()
var cr = []byte("\r")
var lf = []byte("\n")
var ellipses = []byte("[...]\n")

// CurrentIP returns the IP address of the machine we're running on right now.
// The cidr argument can be an empty string, but if set to the CIDR of the
// machine's primary network, it helps us be sure of getting the correct IP
// address (for when there are multiple network interfaces on the machine).
func CurrentIP(cidr string) (string, error) {
	var ipNet *net.IPNet
	if cidr != "" {
		_, ipn, err := net.ParseCIDR(cidr)
		if err == nil {
			ipNet = ipn
		}
		// *** ignoring error since I don't want to change the return value of
		// this method...
	}

	conn, err := net.Dial("udp", "8.8.8.8:80") // doesn't actually connect, dest doesn't need to exist
	if err != nil {
		// fall-back on the old method we had...

		// first just hope http://stackoverflow.com/a/25851186/675083 gives us a
		// cross-linux&MacOS solution that works reliably...
		var out []byte
		out, err = exec.Command("sh", "-c", "ip -4 route get 8.8.8.8 | head -1 | cut -d' ' -f8 | tr -d '\\n'").Output() // #nosec
		var ip string
		if err != nil {
			ip = string(out)

			// paranoid confirmation this ip is in our CIDR
			if ip != "" && ipNet != nil {
				pip := net.ParseIP(ip)
				if pip != nil {
					if !ipNet.Contains(pip) {
						ip = ""
					}
				}
			}
		}

		// if the above fails, fall back on manually going through all our
		// network interfaces
		if ip == "" {
			var addrs []net.Addr
			addrs, err = net.InterfaceAddrs()
			if err != nil {
				return "", err
			}
			for _, address := range addrs {
				if thisIPNet, ok := address.(*net.IPNet); ok && !thisIPNet.IP.IsLoopback() {
					if thisIPNet.IP.To4() != nil {
						if ipNet != nil {
							if ipNet.Contains(thisIPNet.IP) {
								ip = thisIPNet.IP.String()
								break
							}
						} else {
							ip = thisIPNet.IP.String()
							break
						}
					}
				}
			}
		}

		return ip, nil
	}

	defer func() {
		err = conn.Close()
	}()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	ip := localAddr.IP

	// paranoid confirmation this ip is in our CIDR
	if ipNet != nil {
		if ipNet.Contains(ip) {
			return ip.String(), err
		}
	} else {
		return ip.String(), err
	}
	return "", err
}

// byteKey calculates a unique key that describes a byte slice.
func byteKey(b []byte) string {
	l, h := farm.Hash128(b)
	return fmt.Sprintf("%016x%016x", l, h)
}

// copy a file *** should be updated to handle source being on a different
// machine or in an S3-style object store.
func copyFile(source string, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		errc := in.Close()
		if errc != nil {
			if err == nil {
				err = errc
			} else {
				err = fmt.Errorf("%s (and closing source failed: %s)", err.Error(), errc)
			}
		}
	}()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		errc := out.Close()
		if errc != nil {
			if err == nil {
				err = errc
			} else {
				err = fmt.Errorf("%s (and closing dest failed: %s)", err.Error(), errc)
			}
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// compress uses zlib to compress stuff, for transferring big stuff like
// stdout, stderr and environment variables over the network, and for storing
// of same on disk.
func compress(data []byte) ([]byte, error) {
	var compressed bytes.Buffer
	w, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return nil, err
	}
	_, err = w.Write(data)
	if err != nil {
		return nil, err
	}
	err = w.Close()
	if err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

// decompress uses zlib to decompress stuff compressed by compress().
func decompress(compressed []byte) ([]byte, error) {
	b := bytes.NewReader(compressed)
	r, err := zlib.NewReader(b)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(r)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), err
}

// get the current memory usage of a pid, relying on modern linux /proc/*/smaps
// (based on http://stackoverflow.com/a/31881979/675083).
func currentMemory(pid int) (int, error) {
	var err error
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps", pid))
	if err != nil {
		return 0, err
	}
	defer func() {
		errc := f.Close()
		if errc != nil {
			if err == nil {
				err = errc
			} else {
				err = fmt.Errorf("%s (and closing smaps failed: %s)", err.Error(), errc)
			}
		}
	}()

	kb := uint64(0)
	r := bufio.NewScanner(f)
	for r.Scan() {
		line := r.Bytes()
		if bytes.HasPrefix(line, pss) {
			var size uint64
			_, err = fmt.Sscanf(string(line[4:]), "%d", &size)
			if err != nil {
				return 0, err
			}
			kb += size
		}
	}
	if err = r.Err(); err != nil {
		return 0, err
	}

	// convert kB to MB
	mem := int(kb / 1024)

	return mem, err
}

// this prefixSuffixSaver-related code is taken from os/exec, since they are not
// exported. prefixSuffixSaver is an io.Writer which retains the first N bytes
// and the last N bytes written to it. The Bytes() methods reconstructs it with
// a pretty error message.
type prefixSuffixSaver struct {
	N         int
	prefix    []byte
	suffix    []byte
	suffixOff int
	skipped   int64
}

func (w *prefixSuffixSaver) Write(p []byte) (int, error) {
	lenp := len(p)
	p = w.fill(&w.prefix, p)
	if overage := len(p) - w.N; overage > 0 {
		p = p[overage:]
		w.skipped += int64(overage)
	}
	p = w.fill(&w.suffix, p)
	for len(p) > 0 { // 0, 1, or 2 iterations.
		n := copy(w.suffix[w.suffixOff:], p)
		p = p[n:]
		w.skipped += int64(n)
		w.suffixOff += n
		if w.suffixOff == w.N {
			w.suffixOff = 0
		}
	}
	return lenp, nil
}
func (w *prefixSuffixSaver) fill(dst *[]byte, p []byte) []byte {
	if remain := w.N - len(*dst); remain > 0 {
		add := minInt(len(p), remain)
		*dst = append(*dst, p[:add]...)
		p = p[add:]
	}
	return p
}
func (w *prefixSuffixSaver) Bytes() []byte {
	if w.suffix == nil {
		return w.prefix
	}
	if w.skipped == 0 {
		return append(w.prefix, w.suffix...)
	}
	var buf bytes.Buffer
	buf.Grow(len(w.prefix) + len(w.suffix) + 50)
	buf.Write(w.prefix)
	buf.WriteString("\n... omitting ")
	buf.WriteString(strconv.FormatInt(w.skipped, 10))
	buf.WriteString(" bytes ...\n")
	buf.Write(w.suffix[w.suffixOff:])
	buf.Write(w.suffix[:w.suffixOff])
	return buf.Bytes()
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// stdFilter keeps only the first and last line of any contiguous block of \r
// terminated lines (to mostly eliminate progress bars), intended for use with
// stdout/err streaming input, outputting to a prefixSuffixSaver. Because you
// must finish reading from the input before continuing, it returns a channel
// that you should wait to receive an error from (nil if everything workd).
func stdFilter(std io.Reader, out io.Writer) chan error {
	reader := bufio.NewReader(std)
	done := make(chan error)
	go func() {
		var merr *multierror.Error
		for {
			p, err := reader.ReadBytes('\n')

			lines := bytes.Split(p, cr)
			_, errw := out.Write(lines[0])
			if errw != nil {
				merr = multierror.Append(merr, errw)
			}
			if len(lines) > 2 {
				_, errw = out.Write(lf)
				if errw != nil {
					merr = multierror.Append(merr, errw)
				}
				if len(lines) > 3 {
					_, errw = out.Write(ellipses)
					if errw != nil {
						merr = multierror.Append(merr, errw)
					}
				}
				_, errw = out.Write(lines[len(lines)-2])
				if errw != nil {
					merr = multierror.Append(merr, errw)
				}
				_, errw = out.Write(lf)
				if errw != nil {
					merr = multierror.Append(merr, errw)
				}
			}

			if err != nil {
				break
			}
		}
		done <- merr.ErrorOrNil()
	}()
	return done
}

// envOverride deals with values you get from os.Environ, overriding one set
// with values from another. Returns the new slice of environment variables.
func envOverride(orig []string, over []string) []string {
	override := make(map[string]string)
	for _, envvar := range over {
		pair := strings.Split(envvar, "=")
		override[pair[0]] = envvar
	}

	env := orig
	for i, envvar := range env {
		pair := strings.Split(envvar, "=")
		if replace, do := override[pair[0]]; do {
			env[i] = replace
			delete(override, pair[0])
		}
	}

	for _, envvar := range override {
		env = append(env, envvar)
	}
	return env
}

// mkHashedDir uses tohash (which should be a 32 char long string from
// byteKey()) to create a folder nested within baseDir, and in that folder
// creates 2 folders called cwd and tmp, which it returns. Returns an error if
// there were problems making the directories.
func mkHashedDir(baseDir, tohash string) (cwd, tmpDir string, err error) {
	dirs := strings.SplitN(tohash, "", mkHashedLevels)
	dirs, leaf := dirs[0:mkHashedLevels-1], dirs[mkHashedLevels-1]
	dirs = append([]string{baseDir, AppName + "_cwd"}, dirs...)
	dir := filepath.Join(dirs...)
	holdFile := filepath.Join(dir, ".hold")
	defer func() {
		errr := os.Remove(holdFile)
		if errr != nil && !os.IsNotExist(errr) {
			if err == nil {
				err = errr
			} else {
				err = fmt.Errorf("%s (and removing the hold file failed: %s)", err.Error(), errr)
			}
		}
	}()
	tries := 0
	for {
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			tries++
			if tries <= 3 {
				// we retry a few times in case another process is calling
				// rmEmptyDirs on the same baseDir and so conflicting with us
				continue
			}
			return cwd, tmpDir, err
		}

		// and drop a temp file in here so rmEmptyDirs will not immediately
		// remove these dirs
		tries = 0
		var f *os.File
		f, err = os.OpenFile(holdFile, os.O_RDONLY|os.O_CREATE, 0600)
		if err != nil {
			tries++
			if tries <= 3 {
				continue
			}
			return cwd, tmpDir, err
		}
		err = f.Close()
		if err != nil {
			return cwd, tmpDir, err
		}

		break
	}

	// if tohash is a job key then we expect that only 1 of that job is
	// running at any one time per jobqueue, but there could be multiple users
	// running the same cmd, or this user could be running the same command in
	// multiple queues, so we must still create a unique dir at the leaf of our
	// hashed dir structure, to avoid any conflict of multiple processes using
	// the same working directory
	dir, err = ioutil.TempDir(dir, leaf)
	if err != nil {
		return cwd, tmpDir, err
	}

	cwd = filepath.Join(dir, "cwd")
	err = os.Mkdir(cwd, os.ModePerm)
	if err != nil {
		return cwd, tmpDir, err
	}

	tmpDir = filepath.Join(dir, "tmp")
	return cwd, tmpDir, os.Mkdir(tmpDir, os.ModePerm)
}

// rmEmptyDirs deletes leafDir and it's parent directories if they are empty,
// stopping if it reaches baseDir (leaving that undeleted). It's ok if leafDir
// doesn't exist.
func rmEmptyDirs(leafDir, baseDir string) error {
	err := os.Remove(leafDir)
	if err != nil && !os.IsNotExist(err) {
		if strings.Contains(err.Error(), "directory not empty") { //*** not sure where this string comes; probably not cross platform!
			return nil
		}
		return err
	}
	current := leafDir
	parent := filepath.Dir(current)
	for ; parent != baseDir; parent = filepath.Dir(current) {
		thisErr := os.Remove(parent)
		if thisErr != nil {
			// it's expected that we might not be able to delete parents, since
			// some other Job may be running from the same Cwd, meaning this
			// parent dir is not empty
			break
		}
		current = parent
	}
	return nil
}

// removeAllExcept deletes the contents of a given directory (absolute path),
// except for the given folders (relative paths).
func removeAllExcept(path string, exceptions []string) error {
	keepDirs := make(map[string]bool)
	checkDirs := make(map[string]bool)
	path = filepath.Clean(path)
	for _, dir := range exceptions {
		abs := filepath.Join(path, dir)
		keepDirs[abs] = true
		parent := filepath.Dir(abs)
		for {
			if parent == path {
				break
			}
			checkDirs[parent] = true
			parent = filepath.Dir(parent)
		}
	}

	return removeWithExceptions(path, keepDirs, checkDirs)
}

// removeWithExceptions is the recursive part of removeAllExcept's
// implementation that does the real work of deleting stuff.
func removeWithExceptions(path string, keepDirs map[string]bool, checkDirs map[string]bool) error {
	entries, errr := ioutil.ReadDir(path)
	if errr != nil {
		return errr
	}
	for _, entry := range entries {
		abs := filepath.Join(path, entry.Name())
		if !entry.IsDir() {
			err := os.Remove(abs)
			if err != nil {
				return err
			}
			continue
		}

		if keepDirs[abs] {
			continue
		}

		if checkDirs[abs] {
			err := removeWithExceptions(abs, keepDirs, checkDirs)
			if err != nil {
				return err
			}
		} else {
			err := os.RemoveAll(abs)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
