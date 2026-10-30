// Package registry — реестры пакетов алиасов: откуда ставить скиллы.
//
// Реестр — каталог с index.yaml и архивами пакетов: на диске (file://),
// по HTTP или в git-репозитории (Bitbucket, GitLab, что угодно с git
// clone). Индекс называет пакеты, их версии, суммы архивов и отозванные
// версии. Источник с signed_by требует подписи индекса ключом ed25519:
// сумма архива в подписанном индексе — подпись и самого архива.
//
// ochan ходит только в источники из настроек: пакета нет в них — отказ,
// а не поиск где-то ещё. Закрытый контур не должен узнавать, что ochan
// куда-то стучался.
package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Protocol — версия формата реестра и пакета, которую понимает эта сборка.
const Protocol = 1

// IndexName — индекс реестра; IndexSig — его подпись.
const (
	IndexName = "index.yaml"
	IndexSig  = "index.yaml.sig"
)

// Виды источников.
const (
	KindFile = "file"
	KindHTTP = "http"
	KindGit  = "git"
)

// Source — источник из настроек.
type Source struct {
	Name     string
	URL      string
	Kind     string
	SignedBy string
	Priority int
}

// InferKind — вид источника по адресу: file:// и путь — каталог, .git и
// /scm/ (Bitbucket Server) — git, остальное — HTTP.
func InferKind(raw string) string {
	switch {
	case strings.HasPrefix(raw, "file://") || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "."):
		return KindFile
	case strings.HasPrefix(raw, "git+") || strings.HasSuffix(raw, ".git") || strings.Contains(raw, "/scm/"):
		return KindGit
	}
	return KindHTTP
}

// Index — index.yaml реестра.
type Index struct {
	Protocol int               `yaml:"protocol"`
	Packages map[string]*Entry `yaml:"packages"`
}

// Entry — пакет в индексе.
type Entry struct {
	Description string              `yaml:"description,omitempty"`
	Versions    map[string]*Version `yaml:"versions"`
}

// Version — версия пакета.
type Version struct {
	Archive     string    `yaml:"archive"`
	SHA256      string    `yaml:"sha256"`
	Files       int       `yaml:"files"`
	Description string    `yaml:"description,omitempty"`
	Modes       []string  `yaml:"modes,omitempty"`
	Requires    []string  `yaml:"requires,omitempty"`
	Protocol    int       `yaml:"protocol,omitempty"`
	Published   time.Time `yaml:"published,omitempty"`
	// Revoked — почему автор отозвал версию; пусто — не отозвана.
	Revoked string `yaml:"revoked,omitempty"`
}

// PackageProtocol — версия протокола пакета; не указана — первая.
func (v *Version) PackageProtocol() int {
	if v.Protocol == 0 {
		return 1
	}
	return v.Protocol
}

// Sorted — версии пакета от новой к старой.
func (e *Entry) Sorted() []string {
	out := make([]string, 0, len(e.Versions))
	for v := range e.Versions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i], out[j]) > 0 })
	return out
}

// Latest — новейшая неотозванная версия.
func (e *Entry) Latest() (string, *Version) {
	for _, v := range e.Sorted() {
		if e.Versions[v].Revoked == "" {
			return v, e.Versions[v]
		}
	}
	return "", nil
}

// ParseIndex разбирает индекс и проверяет его протокол.
func ParseIndex(data []byte) (*Index, error) {
	var idx Index
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("%s не разбирается: %w", IndexName, err)
	}
	if idx.Protocol == 0 {
		idx.Protocol = 1
	}
	if idx.Protocol > Protocol {
		return nil, fmt.Errorf("реестр протокола v%d, эта сборка понимает v%d — обновите ochan", idx.Protocol, Protocol)
	}
	if idx.Packages == nil {
		idx.Packages = map[string]*Entry{}
	}
	for name, e := range idx.Packages {
		if e == nil || len(e.Versions) == 0 {
			return nil, fmt.Errorf("пакет %s в индексе без версий", name)
		}
		for v, ver := range e.Versions {
			if ver == nil || ver.Archive == "" || ver.SHA256 == "" {
				return nil, fmt.Errorf("пакет %s v%s: нет archive или sha256", name, v)
			}
			if !filepath.IsLocal(ver.Archive) || strings.Contains(ver.Archive, "\\") {
				return nil, fmt.Errorf("пакет %s v%s: архив %q вне реестра", name, v, ver.Archive)
			}
		}
	}
	return &idx, nil
}

// Marshal — индекс текстом.
func (idx *Index) Marshal() ([]byte, error) {
	if idx.Protocol == 0 {
		idx.Protocol = Protocol
	}
	return yaml.Marshal(idx)
}

// ---------------------------------------------------------------------------
// Версии
// ---------------------------------------------------------------------------

var versionText = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,2})$`)

// CleanVersion — 1.4.0 из v1.4.0; не версия — пусто.
func CleanVersion(text string) string {
	m := versionText.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return m[1]
}

// Compare сравнивает версии по частям: 1.10 > 1.9, 1.2 == 1.2.0.
func Compare(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for len(pa) < len(pb) {
		pa = append(pa, "0")
	}
	for len(pb) < len(pa) {
		pb = append(pb, "0")
	}
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Require — зависимость пакета: @имя и условие на версию.
type Require struct {
	Name string
	Op   string
	Want string
}

func (r Require) String() string {
	if r.Want == "" {
		return "@" + r.Name
	}
	return "@" + r.Name + " " + r.Op + r.Want
}

var requirePattern = regexp.MustCompile(`^@?([A-Za-z_][A-Za-z0-9_]*)(?:\s*(>=|<=|>|<|=)?\s*v?(\d+(?:\.\d+){0,2}))?$`)

// ParseRequire разбирает «@pgbase >=1.2».
func ParseRequire(text string) (Require, error) {
	m := requirePattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Require{}, fmt.Errorf("зависимость %q: ожидалось @пакет или @пакет >=1.2", text)
	}
	r := Require{Name: m[1], Op: m[2], Want: m[3]}
	if r.Want != "" && r.Op == "" {
		r.Op = "="
	}
	return r, nil
}

// Satisfied — версия have подходит под условие.
func (r Require) Satisfied(have string) bool {
	if r.Want == "" {
		return true
	}
	c := Compare(have, r.Want)
	switch r.Op {
	case ">=":
		return c >= 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case "<":
		return c < 0
	}
	// «=»: 1.2 — любая 1.2.x, как import в сценарии.
	h, w := strings.Split(have, "."), strings.Split(r.Want, ".")
	if len(h) < len(w) {
		return false
	}
	for i := range w {
		if Compare(h[i], w[i]) != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Клиент
// ---------------------------------------------------------------------------

// Client читает источники.
type Client struct {
	HTTP *http.Client
	// Cache — куда клонируются git-реестры.
	Cache string
	// Keys — открытые ключи подписи по имени (signed_by).
	Keys map[string]ed25519.PublicKey
	// Git — программа git; пусто — git из PATH.
	Git string
}

// Fetched — прочитанный индекс источника.
type Fetched struct {
	Source *Source
	Index  *Index
	// SignedBy — ключ, подпись которым проверена; пусто — источник без
	// подписи.
	SignedBy string
	base     string
}

// Fetch читает индекс источника и проверяет подпись, если она требуется.
func (c *Client) Fetch(ctx context.Context, src *Source) (*Fetched, error) {
	f := &Fetched{Source: src}
	switch src.Kind {
	case KindFile:
		f.base = filePath(src.URL)
	case KindGit:
		dir, err := c.syncGit(ctx, src)
		if err != nil {
			return nil, err
		}
		f.base = dir
	case KindHTTP:
		f.base = strings.TrimRight(src.URL, "/")
	default:
		return nil, fmt.Errorf("источник %s: вид %q — file, http или git", src.Name, src.Kind)
	}
	data, err := c.read(ctx, f, IndexName)
	if err != nil {
		return nil, fmt.Errorf("источник %s: %w", src.Name, err)
	}
	if src.SignedBy != "" {
		key, ok := c.Keys[src.SignedBy]
		if !ok {
			return nil, fmt.Errorf("источник %s подписан ключом %s, а его нет в registry.keys — реестру не верим", src.Name, src.SignedBy)
		}
		sigText, err := c.read(ctx, f, IndexSig)
		if err != nil {
			return nil, fmt.Errorf("источник %s: нет подписи индекса (%s): %w", src.Name, IndexSig, err)
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
		if err != nil || !ed25519.Verify(key, data, sig) {
			return nil, fmt.Errorf("источник %s: подпись индекса не сходится с ключом %s — реестру не верим", src.Name, src.SignedBy)
		}
		f.SignedBy = src.SignedBy
	}
	idx, err := ParseIndex(data)
	if err != nil {
		return nil, fmt.Errorf("источник %s: %w", src.Name, err)
	}
	f.Index = idx
	return f, nil
}

// Archive — архив версии, сверенный с суммой из индекса.
func (c *Client) Archive(ctx context.Context, f *Fetched, v *Version) ([]byte, error) {
	data, err := c.read(ctx, f, v.Archive)
	if err != nil {
		return nil, fmt.Errorf("источник %s: %w", f.Source.Name, err)
	}
	if got := Sum(data); !strings.EqualFold(got, v.SHA256) {
		return nil, fmt.Errorf("источник %s: sha256 архива %s — %s, а в индексе %s: архив подменён или повреждён",
			f.Source.Name, v.Archive, Short(got), Short(v.SHA256))
	}
	return data, nil
}

// maxFile — предел файла из реестра: индекс и архив.
const maxFile = 64 << 20

func (c *Client) read(ctx context.Context, f *Fetched, name string) ([]byte, error) {
	if f.Source.Kind != KindHTTP {
		full := filepath.Join(f.base, filepath.FromSlash(name))
		info, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxFile {
			return nil, fmt.Errorf("%s больше %d МиБ", name, maxFile>>20)
		}
		return os.ReadFile(full)
	}
	u := f.base + "/" + path.Clean("/" + name)[1:]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", redact(u), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("%s больше %d МиБ", name, maxFile>>20)
	}
	return data, nil
}

// syncGit клонирует реестр в кеш или обновляет клон до последней версии.
func (c *Client) syncGit(ctx context.Context, src *Source) (string, error) {
	if c.Cache == "" {
		return "", errors.New("кеш реестров не задан")
	}
	git := c.Git
	if git == "" {
		found, err := exec.LookPath("git")
		if err != nil {
			return "", fmt.Errorf("источник %s — git-репозиторий, а git не установлен", src.Name)
		}
		git = found
	}
	remote := strings.TrimPrefix(src.URL, "git+")
	dir := filepath.Join(c.Cache, src.Name)
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("источник %s: git %s: %v: %s", src.Name, args[0], err, strings.TrimSpace(redact(string(out))))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(c.Cache, 0o700); err != nil {
			return "", err
		}
		_ = os.RemoveAll(dir)
		if err := run("clone", "--depth", "1", "--quiet", remote, dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	if err := run("-C", dir, "fetch", "--depth", "1", "--quiet", "origin", "HEAD"); err != nil {
		return "", err
	}
	if err := run("-C", dir, "reset", "--hard", "--quiet", "FETCH_HEAD"); err != nil {
		return "", err
	}
	return dir, nil
}

// redact прячет пароль в адресе.
func redact(text string) string {
	return regexp.MustCompile(`(://[^/@:\s]+:)[^@\s]+@`).ReplaceAllString(text, "${1}***@")
}

func filePath(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme == "file" {
		return filepath.FromSlash(u.Path)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Суммы и подписи
// ---------------------------------------------------------------------------

// Sum — sha256 в hex.
func Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Short — a3f1…9c2d.
func Short(sum string) string {
	if len(sum) < 12 {
		return sum
	}
	return sum[:4] + "…" + sum[len(sum)-4:]
}

// ParsePublicKey — открытый ключ в base64.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("открытый ключ — 32 байта ed25519 в base64 (ochan '!!registry' keygen)")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey — закрытый ключ в base64.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("закрытый ключ — 64 байта ed25519 в base64 (ochan '!!registry' keygen)")
	}
	return ed25519.PrivateKey(raw), nil
}

// Sign — подпись индекса в base64.
func Sign(key ed25519.PrivateKey, index []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, index)) + "\n")
}

// ---------------------------------------------------------------------------
// Архив пакета
// ---------------------------------------------------------------------------

// packable — файлы пакета, которые идут в архив.
func packable(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".ochan", ".yaml", ".hcplugin", ".md":
		return true
	}
	return false
}

// Pack собирает архив каталога пакета: плоский tar.gz без времён и
// владельцев — один и тот же пакет даёт один и тот же архив и сумму.
func Pack(dir string) ([]byte, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && packable(e.Name()) {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("в %s нет файлов пакета (*.ochan, *.yaml, *.hcplugin)", dir)
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		mode := int64(0o644)
		if filepath.Ext(name) == ".hcplugin" {
			mode = 0o755
		}
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), files, nil
}

// Unpack распаковывает архив пакета в dest. Только плоские обычные файлы
// пакета: путь с каталогом, ссылка или устройство — отказ, а не попытка
// угадать, куда их положить.
func Unpack(data []byte, dest string) ([]string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("архив пакета не gzip: %w", err)
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, err
	}
	var files []string
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("архив пакета повреждён: %w", err)
		}
		name := hdr.Name
		if hdr.Typeflag != tar.TypeReg || strings.ContainsAny(name, "/\\") || name == "" || name[0] == '.' || !packable(name) {
			return nil, fmt.Errorf("в архиве пакета %q — не файл пакета: архив плоский, только *.ochan, *.yaml, *.hcplugin, *.md", name)
		}
		total += hdr.Size
		if total > maxFile {
			return nil, fmt.Errorf("архив пакета больше %d МиБ в распакованном виде", maxFile>>20)
		}
		out, err := os.OpenFile(filepath.Join(dest, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		return nil, errors.New("архив пакета пуст")
	}
	return files, nil
}
