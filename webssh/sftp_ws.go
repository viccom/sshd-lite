//go:build webssh

package webssh

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/jpillora/sftp"
)

// toSFTPPath adapts a browser-visible path for the SFTP wire. On Windows,
// drive-letter paths ("C:/x") must carry the leading-slash SFTP convention
// ("/C:/x"); the sftp server otherwise treats them as workdir-relative and
// joins them under its working directory. POSIX paths pass through
// unchanged (mole only ever runs on POSIX hosts, hence the gap).
func toSFTPPath(p string) string {
	if runtime.GOOS != "windows" || len(p) < 2 || p[1] != ':' {
		return p
	}
	c := p[0]
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return "/" + p
	}
	return p
}

// sftpClient lazy-builds the SFTP client on the session's loopback SSH
// client. It is single-session: the mutex guards concurrent file requests.
func (s *wsSession) sftpClient() (*sftp.Client, error) {
	s.sftpMu.Lock()
	defer s.sftpMu.Unlock()
	if s.sftp != nil {
		if _, err := s.sftp.Getwd(); err == nil {
			return s.sftp, nil
		}
		s.sftp.Close()
		s.sftp = nil
	}
	sc, err := sftp.NewClient(s.sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp new client: %w", err)
	}
	s.sftp = sc
	return sc, nil
}

func (s *wsSession) handleFileMsg(typ byte, payload []byte) {
	switch typ {
	case msgFileListReq:
		s.fileList(payload)
	case msgFileUploadReq, msgFileUploadData, msgFileUploadEnd:
		s.fileUpload(typ, payload)
	case msgFileDownloadReq:
		s.fileDownload(payload, "download")
	case msgFileReadReq:
		s.fileDownload(payload, "read")
	case msgFileDeleteReq:
		s.fileDelete(payload)
	case msgFileMkdirReq:
		s.fileMkdir(payload)
	}
}

func (s *wsSession) ack(ok bool, msg string, extra map[string]any) {
	m := map[string]any{"ok": ok, "msg": msg}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	_ = s.w.writeFrame(msgFileAckResp, b)
}

func (s *wsSession) fileList(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil {
		s.ack(false, "invalid request", nil)
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	req.Path = toSFTPPath(req.Path)
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	files, err := sc.ReadDir(req.Path)
	if err != nil {
		s.ack(false, fmt.Sprintf("read dir: %v", err), nil)
		return
	}
	home, _ := sc.Getwd()
	if home == "" {
		home = "/"
	}
	type entry struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Mode    string `json:"mode"`
		ModTime string `json:"mod_time"`
		IsDir   bool   `json:"is_dir"`
	}
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		entries = append(entries, entry{
			Name: f.Name(), Size: f.Size(), Mode: f.Mode().String(),
			ModTime: f.ModTime().Format("2006-01-02 15:04:05"), IsDir: f.IsDir(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	build := func(es []entry, truncated bool) ([]byte, error) {
		m := map[string]any{"path": req.Path, "home": home, "files": es}
		if truncated {
			m["truncated"] = true
		}
		return json.Marshal(m)
	}
	resp, err := build(entries, false)
	const maxPayload = 65535
	for err == nil && len(resp) > maxPayload && len(entries) > 1 {
		entries = entries[:len(entries)*3/4]
		resp, err = build(entries, true)
	}
	if err != nil {
		s.ack(false, "marshal list", nil)
		return
	}
	_ = s.w.writeFrame(msgFileListResp, resp)
}

func (s *wsSession) fileUpload(typ byte, payload []byte) {
	switch typ {
	case msgFileUploadReq:
		var req struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(payload, &req) != nil || req.Path == "" {
			s.ack(false, "invalid upload request", nil)
			return
		}
		sc, err := s.sftpClient()
		if err != nil {
			s.ack(false, err.Error(), nil)
			return
		}
		f, err := sc.Create(toSFTPPath(req.Path))
		if err != nil {
			s.ack(false, fmt.Sprintf("create file: %v", err), nil)
			return
		}
		s.uploadMu.Lock()
		// Defensive: a second REQ without END replaces (and closes) the
		// previous in-flight upload instead of leaking its file handle.
		if s.uploadFile != nil {
			s.uploadFile.Close()
		}
		s.uploadFile = f
		s.uploadErr = nil
		s.uploadMu.Unlock()
	case msgFileUploadData:
		s.uploadMu.Lock()
		if s.uploadFile != nil && len(payload) > 0 {
			_, s.uploadErr = s.uploadFile.Write(payload)
		}
		s.uploadMu.Unlock()
	case msgFileUploadEnd:
		s.uploadMu.Lock()
		f := s.uploadFile
		werr := s.uploadErr
		s.uploadFile = nil
		s.uploadMu.Unlock()
		if f == nil {
			s.ack(false, "no upload in progress", nil)
			return
		}
		var size int64
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		f.Close()
		if werr != nil {
			s.ack(false, fmt.Sprintf("write error: %v", werr), nil)
			return
		}
		s.ack(true, "upload completed", map[string]any{"size": size})
	}
}

func (s *wsSession) fileDownload(payload []byte, mode string) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" {
		s.ack(false, "invalid request", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	f, err := sc.Open(toSFTPPath(req.Path))
	if err != nil {
		s.ack(false, fmt.Sprintf("open file: %v", err), nil)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.ack(false, fmt.Sprintf("stat file: %v", err), nil)
		return
	}
	buf := make([]byte, 32768)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if werr := s.w.writeFrame(msgFileDataResp, buf[:n]); werr != nil {
				return
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			s.ack(false, fmt.Sprintf("read file: %v", err), nil)
			return
		}
	}
	extra := map[string]any{"size": st.Size(), "name": filepath.Base(req.Path)}
	if mode == "read" {
		extra["mode"] = "read"
	}
	s.ack(true, mode+" completed", extra)
}

func (s *wsSession) fileDelete(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" || req.Path == "/" {
		s.ack(false, "invalid path", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	if err := sc.RemoveAll(toSFTPPath(req.Path)); err != nil {
		s.ack(false, fmt.Sprintf("delete: %v", err), nil)
		return
	}
	s.ack(true, "deleted", nil)
}

func (s *wsSession) fileMkdir(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" {
		s.ack(false, "invalid request", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	if err := sc.MkdirAll(toSFTPPath(req.Path)); err != nil {
		s.ack(false, fmt.Sprintf("mkdir: %v", err), nil)
		return
	}
	s.ack(true, "created", nil)
}
