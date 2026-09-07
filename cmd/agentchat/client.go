package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/agentchat/pkg/envx"
)

// The client directory carries the brand; legacyDir stays readable until the
// whole fleet has migrated.
const (
	brandDir  = "openflock"
	legacyDir = "agentchat"
)

// profile is the saved identity for one room membership.
type profile struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	Room    string `json:"room_name"`
	Name    string `json:"name"`
	JoinURL string `json:"join_url,omitempty"`
}

// profileDir is where a new profile is written: ~/.openflock, unless this
// install predates the rename and still keeps its profiles in ~/.agentchat.
// The test is whether a directory HOLDS profiles, not whether it exists: the
// join instructions mkdir ~/.openflock for cli.sh, and an empty new directory
// must not hide saved identities. Nothing here moves or copies a profile;
// they are token files, so that move stays the human's.
func profileDir() string {
	if d := envx.Get("HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "." + brandDir
	}
	newDir := filepath.Join(home, "."+brandDir)
	if hasProfiles(newDir) {
		return newDir
	}
	legacy := filepath.Join(home, "."+legacyDir)
	if hasProfiles(legacy) {
		return legacy
	}
	return newDir
}

func hasProfiles(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return len(m) > 0
}

// profilePath finds an existing profile in either directory, so an identity
// saved before the rename still loads once the new directory has others in it.
func profilePath(name string) string {
	p := filepath.Join(profileDir(), name+".json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if envx.Get("HOME") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			legacy := filepath.Join(home, "."+legacyDir, name+".json")
			if _, err := os.Stat(legacy); err == nil {
				return legacy
			}
		}
	}
	return p
}

func loadProfile(name string) (profile, error) {
	var p profile
	raw, err := os.ReadFile(profilePath(name))
	if err != nil {
		return p, fmt.Errorf("no profile %q — run `agentchat join` first (%w)", name, err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	return p, nil
}

func saveProfile(name string, p profile) error {
	if err := os.MkdirAll(profileDir(), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(profilePath(name), raw, 0o600)
}

type client struct {
	server string
	token  string
	http   *http.Client
}

func newClient(p profile) *client {
	return &client{server: p.Server, token: p.Token,
		http: &http.Client{Timeout: 60 * time.Second}}
}

// anonClient talks to a server without a participant token (create/join).
func anonClient(server string) *client {
	return &client{server: server, http: &http.Client{Timeout: 60 * time.Second}}
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Msg)
}

func (c *client) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.server+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = string(raw)
		}
		return &apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *client) upload(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	mw.Close()

	req, err := http.NewRequest("POST", c.server+"/api/v1/attachments", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload failed (%d): %s", resp.StatusCode, out.Error)
	}
	return out.ID, nil
}

// setAvatar uploads an image as the caller's profile picture.
func (c *client) setAvatar(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	mw.Close()

	req, err := http.NewRequest("POST", c.server+"/api/v1/me/avatar", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("avatar upload failed (%d): %s", resp.StatusCode, raw)
	}
	return nil
}

func (c *client) download(id string, w io.Writer) (filename string, err error) {
	req, err := http.NewRequest("GET", c.server+"/api/v1/attachments/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("download failed (%d): %s", resp.StatusCode, raw)
	}
	_, err = io.Copy(w, resp.Body)
	return resp.Header.Get("Content-Disposition"), err
}
