package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	configVersion      = 1
	passwordIterations = 310000
	passwordKeyLength  = 32
)

type passwordRecord struct {
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

type Config struct {
	Version  int            `json:"version"`
	Username string         `json:"username"`
	Password passwordRecord `json:"password"`
	ShareDir string         `json:"shareDir"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("配置文件格式错误: %w", err)
	}
	if err := validateStoredConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateStoredConfig(cfg *Config) error {
	if cfg.Version != configVersion {
		return fmt.Errorf("不支持的配置版本: %d", cfg.Version)
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.ShareDir == "" {
		return errors.New("配置缺少账号或共享目录")
	}
	if cfg.Password.Algorithm != "pbkdf2-sha256" || cfg.Password.Iterations < 100000 {
		return errors.New("密码配置无效")
	}
	if _, err := base64.RawStdEncoding.DecodeString(cfg.Password.Salt); err != nil {
		return errors.New("密码盐值无效")
	}
	if _, err := base64.RawStdEncoding.DecodeString(cfg.Password.Hash); err != nil {
		return errors.New("密码摘要无效")
	}
	abs, err := filepath.Abs(cfg.ShareDir)
	if err != nil {
		return fmt.Errorf("共享目录无效: %w", err)
	}
	cfg.ShareDir = filepath.Clean(abs)
	return nil
}

func saveConfig(path string, cfg *Config) error {
	if err := validateStoredConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func newPasswordRecord(password string) (passwordRecord, error) {
	if err := validatePassword(password); err != nil {
		return passwordRecord{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return passwordRecord{}, err
	}
	hash := pbkdf2SHA256([]byte(password), salt, passwordIterations, passwordKeyLength)
	return passwordRecord{
		Algorithm:  "pbkdf2-sha256",
		Iterations: passwordIterations,
		Salt:       base64.RawStdEncoding.EncodeToString(salt),
		Hash:       base64.RawStdEncoding.EncodeToString(hash),
	}, nil
}

func validatePassword(password string) error {
	if len([]rune(password)) < 8 {
		return errors.New("密码至少需要 8 个字符")
	}
	if len(password) > 256 {
		return errors.New("密码过长")
	}
	return nil
}

func verifyPassword(record passwordRecord, password string) bool {
	if record.Algorithm != "pbkdf2-sha256" || record.Iterations < 100000 || record.Iterations > 2000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(record.Salt)
	if err != nil || len(salt) < 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(record.Hash)
	if err != nil || len(expected) != passwordKeyLength {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, record.Iterations, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	const hashLength = 32
	blocks := (keyLength + hashLength - 1) / hashLength
	result := make([]byte, 0, blocks*hashLength)
	buf := make([]byte, len(salt)+4)
	copy(buf, salt)

	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(buf[len(salt):], uint32(block))
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(buf)
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)

		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		result = append(result, t...)
	}
	return result[:keyLength]
}

func randomToken(byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
