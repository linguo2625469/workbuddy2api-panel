package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type ImportResult struct {
	Added   int      `json:"added"`
	Skipped int      `json:"skipped"`
	NoKey   int      `json:"no_key"`
	Total   int      `json:"total"`
	File    string   `json:"file,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

func sqliteRO(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file:" + filepath.ToSlash(abs) + "?mode=ro"
}

func importFile(r *http.Request) (path, cleanup string, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return "", "", fmt.Errorf("upload: %w", err)
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			return "", "", fmt.Errorf("pick a .db file")
		}
		defer f.Close()
		tmp, err := os.CreateTemp("", "zcode-mgr-*.db")
		if err != nil {
			return "", "", err
		}
		if _, err := io.Copy(tmp, f); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", "", err
		}
		tmp.Close()
		name := tmp.Name()
		if hdr != nil && hdr.Filename != "" {
			_ = hdr.Filename
		}
		return name, name, nil
	}
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Path) == "" {
		return "", "", fmt.Errorf("no file")
	}
	return body.Path, "", nil
}

func (s *Server) ImportManagerDB(path string) (ImportResult, error) {
	path = strings.TrimSpace(path)
	res := ImportResult{File: filepath.Base(path)}
	if path == "" {
		return res, fmt.Errorf("no file")
	}
	if _, err := os.Stat(path); err != nil {
		return res, fmt.Errorf("manager db: %w", err)
	}
	src, err := sql.Open("sqlite", sqliteRO(path))
	if err != nil {
		return res, err
	}
	defer src.Close()
	src.SetMaxOpenConns(1)

	rows, err := src.Query(`SELECT TRIM(COALESCE(email,'')), TRIM(COALESCE(zcodeApiKey,'')), COALESCE(id,'') FROM accounts`)
	if err != nil {
		// schema without the manager row id (fixtures/older exports): import
		// without the gateway device-mid pin
		rows, err = src.Query(`SELECT TRIM(COALESCE(email,'')), TRIM(COALESCE(zcodeApiKey,'')), '' FROM accounts`)
		if err != nil {
			return res, fmt.Errorf("not a manager accounts.db (need zcodeApiKey): %w", err)
		}
	}
	defer rows.Close()

	for rows.Next() {
		var email, key, rowID string
		if err := rows.Scan(&email, &key, &rowID); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Total++
		if key == "" {
			res.NoKey++
			continue
		}
		if s.store.HasJWT(key) {
			res.Skipped++
			continue
		}
		if _, err := s.store.Add(key, email); err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", email, err))
			continue
		}
		// The device identity is the manager gateway's mid for this account —
		// uuid5(DNS, "zcode-gw."+<manager row id>) — the (account, device)
		// pair the edge already knows. A fresh uuid instead is a brand-new
		// device and gets 405/3012 "unusual activity" (proven 2026-09-11:
		// same request + the real mid → 200).
		if rowID != "" {
			mid := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("zcode-gw."+rowID)).String()
			s.store.SetDeviceMid(key, mid)
		}
		res.Added++
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	return res, nil
}
