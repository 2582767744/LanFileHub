package main

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxUploadBytes = int64(20 * 1024 * 1024 * 1024)
	maxNameRunes   = 240
)

type fileEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"isDir"`
	IsLink    bool      `json:"isLink"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Extension string    `json:"extension,omitempty"`
}

func cleanRelativePath(input string) (string, error) {
	if len(input) > 4096 || strings.ContainsRune(input, 0) {
		return "", errors.New("路径无效")
	}
	if filepath.IsAbs(input) || filepath.VolumeName(input) != "" || strings.HasPrefix(input, "/") || strings.HasPrefix(input, "\\") {
		return "", errors.New("不允许使用绝对路径")
	}
	normalized := strings.ReplaceAll(input, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return "", errors.New("路径不能跳出共享目录")
		}
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+normalized), "/")
	if cleaned == "." {
		cleaned = ""
	}
	return filepath.FromSlash(cleaned), nil
}

func ensureWithin(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("路径超出共享目录")
	}
	return nil
}

func secureExistingPath(root, relative string) (string, error) {
	cleaned, err := cleanRelativePath(relative)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("共享目录不可用: %w", err)
	}
	candidate := filepath.Join(rootAbs, cleaned)
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if err := ensureWithin(rootReal, real); err != nil {
		return "", err
	}
	return candidate, nil
}

func secureNewChildPath(root, parentRelative, name string) (string, error) {
	if err := validateFileName(name); err != nil {
		return "", err
	}
	parent, err := secureExistingPath(root, parentRelative)
	if err != nil {
		return "", err
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return "", errors.New("目标文件夹不存在")
	}
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if err := ensureWithin(rootReal, parentReal); err != nil {
		return "", err
	}
	return filepath.Join(parentReal, name), nil
}

func validateFileName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) {
		return errors.New("文件名不能为空")
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return errors.New("文件名过长")
	}
	if strings.ContainsAny(name, `<>:"/\|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("文件名包含 Windows 不支持的字符")
	}
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	reserved := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
		"COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
		"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
	if reserved[base] {
		return errors.New("该文件名是 Windows 保留名称")
	}
	return nil
}

func apiPath(parent, name string) string {
	joined := path.Join("/", filepath.ToSlash(parent), name)
	return strings.TrimPrefix(joined, "/")
}

func entryFromInfo(parent string, info os.FileInfo, isLink bool) fileEntry {
	ext := ""
	if !info.IsDir() {
		ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(info.Name())), ".")
	}
	return fileEntry{
		Name:      info.Name(),
		Path:      apiPath(parent, info.Name()),
		IsDir:     info.IsDir(),
		IsLink:    isLink,
		Size:      info.Size(),
		Modified:  info.ModTime(),
		Extension: ext,
	}
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	relative, err := cleanRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := secureExistingPath(a.shareDirectory(), relative)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "文件夹不存在")
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		writeAPIError(w, http.StatusBadRequest, "目标不是文件夹")
		return
	}
	items, err := os.ReadDir(target)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "无法读取该文件夹")
		return
	}
	entries := make([]fileEntry, 0, len(items))
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".lanfilehub-upload-") {
			continue
		}
		itemInfo, err := item.Info()
		if err != nil {
			continue
		}
		entries = append(entries, entryFromInfo(filepath.ToSlash(relative), itemInfo, item.Type()&os.ModeSymlink != 0))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    filepath.ToSlash(relative),
		"entries": entries,
	})
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	relative := r.URL.Query().Get("path")
	target, err := secureExistingPath(a.shareDirectory(), relative)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "文件不存在")
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		writeAPIError(w, http.StatusBadRequest, "目标不是文件")
		return
	}
	file, err := os.Open(target)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, "无法读取文件")
		return
	}
	defer file.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()})
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	select {
	case a.uploadSlots <- struct{}{}:
		defer func() { <-a.uploadSlots }()
	default:
		writeAPIError(w, http.StatusTooManyRequests, "当前上传任务较多，请稍后再试")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "上传请求格式错误")
		return
	}
	parent := r.URL.Query().Get("path")
	overwrite := r.URL.Query().Get("overwrite") == "1"
	var uploaded *fileEntry

	for partCount := 0; partCount < 16; partCount++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "读取上传内容失败")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		if uploaded != nil {
			part.Close()
			writeAPIError(w, http.StatusBadRequest, "每次请求只能上传一个文件")
			return
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		destination, err := secureNewChildPath(a.shareDirectory(), parent, name)
		if err != nil {
			part.Close()
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if existing, err := os.Stat(destination); err == nil {
			if existing.IsDir() {
				part.Close()
				writeAPIError(w, http.StatusConflict, "同名文件夹已经存在")
				return
			}
			if !overwrite {
				part.Close()
				writeAPIError(w, http.StatusConflict, "同名文件已经存在")
				return
			}
		}

		tmp, err := os.CreateTemp(filepath.Dir(destination), ".lanfilehub-upload-*")
		if err != nil {
			part.Close()
			writeAPIError(w, http.StatusForbidden, "无法在目标目录创建文件")
			return
		}
		tmpName := tmp.Name()
		completed := false
		defer func() {
			if !completed {
				_ = os.Remove(tmpName)
			}
		}()

		limited := &io.LimitedReader{R: part, N: maxUploadBytes + 1}
		written, copyErr := io.Copy(tmp, limited)
		part.Close()
		if copyErr == nil {
			copyErr = tmp.Sync()
		}
		closeErr := tmp.Close()
		if copyErr != nil || closeErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "写入文件失败")
			return
		}
		if written > maxUploadBytes {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "单个文件不能超过 20GB")
			return
		}
		if err := replaceFile(tmpName, destination); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "保存文件失败")
			return
		}
		completed = true
		info, err := os.Stat(destination)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "文件已保存但无法读取属性")
			return
		}
		entry := entryFromInfo(filepath.ToSlash(parent), info, false)
		uploaded = &entry
		a.audit(r, "upload", entry.Path)
	}
	if uploaded == nil {
		writeAPIError(w, http.StatusBadRequest, "没有找到要上传的文件")
		return
	}
	writeJSON(w, http.StatusCreated, uploaded)
}

func (a *App) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	target, err := secureNewChildPath(a.shareDirectory(), input.Path, input.Name)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Mkdir(target, 0755); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeAPIError(w, http.StatusConflict, "同名文件或文件夹已经存在")
		} else {
			writeAPIError(w, http.StatusForbidden, "创建文件夹失败")
		}
		return
	}
	a.audit(r, "mkdir", apiPath(filepath.ToSlash(input.Path), input.Name))
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (a *App) handleRename(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	cleaned, err := cleanRelativePath(input.Path)
	if err != nil || cleaned == "" {
		writeAPIError(w, http.StatusBadRequest, "不能重命名共享根目录")
		return
	}
	source, err := secureExistingPath(a.shareDirectory(), cleaned)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info, err := os.Lstat(source); err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeAPIError(w, http.StatusBadRequest, "暂不支持重命名链接文件")
		return
	}
	parentRelative := filepath.ToSlash(filepath.Dir(cleaned))
	if parentRelative == "." {
		parentRelative = ""
	}
	destination, err := secureNewChildPath(a.shareDirectory(), parentRelative, input.NewName)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(destination); err == nil {
		writeAPIError(w, http.StatusConflict, "同名文件或文件夹已经存在")
		return
	}
	if err := os.Rename(source, destination); err != nil {
		writeAPIError(w, http.StatusForbidden, "重命名失败")
		return
	}
	a.audit(r, "rename", cleaned+" -> "+apiPath(parentRelative, input.NewName))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	cleaned, err := cleanRelativePath(input.Path)
	if err != nil || cleaned == "" {
		writeAPIError(w, http.StatusBadRequest, "不能删除共享根目录")
		return
	}
	target, err := secureExistingPath(a.shareDirectory(), cleaned)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeAPIError(w, http.StatusBadRequest, "暂不支持删除链接文件")
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeAPIError(w, http.StatusForbidden, "删除失败")
		return
	}
	a.audit(r, "delete", cleaned)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
