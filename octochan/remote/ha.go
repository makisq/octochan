package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Пара серверов: primary и standby.
//
// Консенсуса нет — и не обещается. Заявкой, журналом, ролями владеет один
// узел — primary: только он пишет. Standby держит копию: раз в
// replicate_every забирает у primary опись файлов состояния и те, что
// изменились; на запись клиенту отвечает отказом с адресом primary, а
// чтение выполняет по копии.
//
// Переключение — решение человека: ochan ha promote на standby делает его
// primary с эпохой на единицу больше. Эпоха — забор: узел, увидевший у
// другого primary эпоху больше своей, перестаёт быть primary сам, а то,
// что успел записать после разрыва, не сливает и не стирает, а
// откладывает в ha/diverged-…: решать, что с этим делать, — человеку.
// Два primary одной эпохи — конфликт: оба отказывают в записи, пока
// человек не повысит одного.
//
// Выполнение заявки не переживает переключение молча: перед выполнением
// заявка помечается «выполняется», и primary ждёт, пока пометка дойдёт до
// standby (барьер). Новый primary находит такие заявки и объявляет их
// исход неизвестным — второй раз они не выполняются.

const (
	RolePrimary = "primary"
	RoleStandby = "standby"
)

// HAState — роль узла; лежит в ha.json узла и не копируется.
type HAState struct {
	Node  string    `json:"node"`
	Role  string    `json:"role"`
	Epoch int64     `json:"epoch"`
	Since time.Time `json:"since"`
	By    string    `json:"by,omitempty"`
	// Diverged — отложенные расходящиеся состояния, относительно рабочего
	// каталога.
	Diverged []string `json:"diverged,omitempty"`
}

// LoadHAState читает ha.json.
func LoadHAState(path string) (*HAState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st HAState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s испорчен: %w", path, err)
	}
	if st.Role != RolePrimary && st.Role != RoleStandby {
		return nil, fmt.Errorf("%s: роль узла %q — primary или standby", path, st.Role)
	}
	return &st, nil
}

// SaveHAState записывает ha.json целиком или никак.
func SaveHAState(path string, st *HAState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data, 0o600)
}

// Promote делает узел primary. known — наибольшая эпоха, которую узел
// видел у другого; новая эпоха больше обеих.
func Promote(path, by string, known int64) (*HAState, error) {
	st, err := LoadHAState(path)
	if err != nil {
		return nil, err
	}
	if st.Role == RolePrimary && st.Epoch >= known {
		return nil, fmt.Errorf("узел %s уже primary (эпоха %d)", st.Node, st.Epoch)
	}
	epoch := st.Epoch
	if known > epoch {
		epoch = known
	}
	st.Role, st.Epoch, st.Since, st.By = RolePrimary, epoch+1, time.Now().UTC(), by
	return st, SaveHAState(path, st)
}

// HAConfig — настройки пары.
type HAConfig struct {
	// StatePath — ha.json узла.
	StatePath string
	// Base — рабочий каталог; Paths — что из него копируется: файлы и
	// каталоги, относительно Base.
	Base  string
	Paths []string
	// Self — адрес этого узла для клиентов; Peer — адрес другого узла.
	Self string
	Peer string
	// PeerTLS — как ходить к другому узлу.
	PeerTLS        Settings
	AllowPlaintext bool
	// Token — общий секрет пары: им узлы подтверждают друг другу, что
	// они пара.
	Token string
	// Every — как часто standby забирает копию и узлы сверяют эпохи.
	Every time.Duration
	// RequireStandby — без подтверждения резерва выполнение не начинается.
	RequireStandby bool
	Logger         *log.Logger
}

// HAStatus — что узел говорит о себе.
type HAStatus struct {
	Node    string `json:"node"`
	Role    string `json:"role"`
	Epoch   int64  `json:"epoch"`
	Self    string `json:"self,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Primary string `json:"primary,omitempty"`
	// Conflict — два primary одной эпохи: запись закрыта до решения.
	Conflict bool `json:"conflict,omitempty"`
	// LastSync — когда standby последний раз получил копию.
	LastSync time.Time `json:"last_sync,omitempty"`
	Error    string    `json:"error,omitempty"`
	Diverged []string  `json:"diverged,omitempty"`
}

// Writable — узел принимает запись: primary без конфликта.
func (s HAStatus) Writable() bool { return s.Role == RolePrimary && !s.Conflict }

// HA — узел пары.
type HA struct {
	cfg     HAConfig
	client  *http.Client
	scheme  string
	barrier string

	mu       sync.Mutex
	state    HAState
	modTime  time.Time
	conflict bool
	lastSync time.Time
	lastErr  string
	syncMu   sync.Mutex
}

// NewHA открывает узел пары. ha.json должен быть: его создаёт ochan ha init.
func NewHA(cfg HAConfig) (*HA, error) {
	if cfg.Token == "" {
		return nil, errors.New("у пары нет общего секрета (ha.token_file): узлы не отличили бы друг друга от чужих")
	}
	if cfg.Every <= 0 {
		cfg.Every = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	h := &HA{cfg: cfg}
	if err := h.reload(true); err != nil {
		return nil, err
	}
	if cfg.Peer != "" {
		host, _, err := net.SplitHostPort(cfg.Peer)
		if err != nil {
			return nil, fmt.Errorf("адрес второго узла %q: нужен вид host:port", cfg.Peer)
		}
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}
		h.scheme = "http"
		if cfg.PeerTLS.SSL {
			tlsConfig, err := ClientTLS(cfg.PeerTLS, host)
			if err != nil {
				return nil, err
			}
			transport.TLSClientConfig = tlsConfig
			h.scheme = "https"
		} else if !isLoopback(cfg.Peer) && !cfg.AllowPlaintext {
			return nil, fmt.Errorf("ssl выключен, а второй узел %s — другая машина: состояние пары, с ролями и "+
				"паролями, шло бы открытым текстом", cfg.Peer)
		}
		h.client = &http.Client{Transport: transport, Timeout: time.Minute}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	h.barrier = hex.EncodeToString(buf)
	return h, nil
}

// reload перечитывает ha.json, если он изменился: ochan ha promote
// пишет его, не останавливая сервер.
func (h *HA) reload(force bool) error {
	info, err := os.Stat(h.cfg.StatePath)
	if err != nil {
		return fmt.Errorf("узел не настроен (%s): ochan ha init: %w", h.cfg.StatePath, err)
	}
	h.mu.Lock()
	same := !force && info.ModTime().Equal(h.modTime)
	h.mu.Unlock()
	if same {
		return nil
	}
	st, err := LoadHAState(h.cfg.StatePath)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if st.Epoch != h.state.Epoch || st.Role != h.state.Role {
		h.conflict = false
	}
	h.state, h.modTime = *st, info.ModTime()
	h.mu.Unlock()
	return nil
}

// Status — состояние узла.
func (h *HA) Status() HAStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := HAStatus{Node: h.state.Node, Role: h.state.Role, Epoch: h.state.Epoch, Self: h.cfg.Self, Peer: h.cfg.Peer,
		Conflict: h.conflict, LastSync: h.lastSync, Error: h.lastErr, Diverged: h.state.Diverged}
	if st.Role == RolePrimary {
		st.Primary = h.cfg.Self
	} else {
		st.Primary = h.cfg.Peer
	}
	return st
}

// Writable — узел принимает запись.
func (h *HA) Writable() bool { return h.Status().Writable() }

// BarrierToken — ключ барьера для дочерних процессов этого узла.
func (h *HA) BarrierToken() string { return h.barrier }

// Run — работа узла до отмены ctx: standby забирает копию, оба сверяют
// эпохи со вторым узлом.
func (h *HA) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	next := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if err := h.reload(false); err != nil {
				h.setErr(err)
				continue
			}
			if now.Before(next) {
				continue
			}
			next = now.Add(h.cfg.Every)
			h.step(ctx)
		}
	}
}

func (h *HA) setErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.lastErr = ""
		return
	}
	if h.lastErr != err.Error() {
		h.cfg.Logger.Printf("пара: %v", err)
	}
	h.lastErr = err.Error()
}

// step — один шаг: узнать, что со вторым узлом, и сделать, что положено.
func (h *HA) step(ctx context.Context) {
	if h.client == nil {
		return
	}
	peer, err := h.peerStatus(ctx)
	me := h.Status()
	if err != nil {
		if me.Role == RoleStandby {
			h.setErr(fmt.Errorf("primary %s недоступен: %w; копия от %s", h.cfg.Peer, err, formatTime(me.LastSync)))
		} else {
			h.setErr(fmt.Errorf("резервный узел %s недоступен: %w", h.cfg.Peer, err))
		}
		return
	}
	switch {
	case me.Role == RolePrimary && peer.Role == RolePrimary && peer.Epoch > me.Epoch:
		h.demote(peer.Epoch, fmt.Sprintf("узел %s стал primary с эпохой %d", peer.Node, peer.Epoch))
	case me.Role == RolePrimary && peer.Role == RolePrimary && peer.Epoch == me.Epoch:
		h.mu.Lock()
		h.conflict = true
		h.mu.Unlock()
		h.setErr(fmt.Errorf("конфликт: узел %s тоже primary с той же эпохой %d — запись закрыта на обоих, "+
			"пока один не повышен (ochan ha promote)", peer.Node, me.Epoch))
	case me.Role == RolePrimary && peer.Role == RolePrimary:
		// У второго эпоха меньше: это бывший primary после разрыва.
		// Сказать ему — он отступит сам.
		_ = h.tellPeer(ctx, me.Epoch)
		h.setErr(nil)
	case me.Role == RolePrimary:
		h.setErr(nil)
	case peer.Role != RolePrimary:
		h.setErr(fmt.Errorf("primary нет: второй узел %s тоже standby — ochan ha promote на одном из них", peer.Node))
	default:
		if err := h.pull(ctx); err != nil {
			h.setErr(err)
			return
		}
		h.setErr(nil)
	}
}

// demote — узел перестаёт быть primary. Своё состояние он откладывает:
// то, что записано после разрыва, копия от нового primary затёрла бы.
func (h *HA) demote(epoch int64, why string) {
	h.mu.Lock()
	st := h.state
	h.mu.Unlock()
	stamp := time.Now().UTC().Format("20060102T150405")
	dir := filepath.Join("ha", fmt.Sprintf("diverged-e%d-%s", st.Epoch, stamp))
	if err := h.copyState(filepath.Join(h.cfg.Base, dir)); err != nil {
		h.cfg.Logger.Printf("пара: отложить состояние перед отступлением не удалось: %v — остаюсь primary", err)
		return
	}
	st.Role, st.Epoch, st.Since, st.By = RoleStandby, epoch, time.Now().UTC(), why
	st.Diverged = append(st.Diverged, dir)
	if err := SaveHAState(h.cfg.StatePath, &st); err != nil {
		h.cfg.Logger.Printf("пара: %v", err)
		return
	}
	h.cfg.Logger.Printf("пара: %s — этот узел теперь standby; прежнее состояние отложено в %s", why, dir)
	_ = h.reload(true)
}

// ---------------------------------------------------------------------------
// Опись и копия
// ---------------------------------------------------------------------------

// FileEntry — файл состояния в описи.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Sum  string `json:"sum"`
}

// Manifest — опись состояния primary.
type Manifest struct {
	Node  string      `json:"node"`
	Epoch int64       `json:"epoch"`
	Files []FileEntry `json:"files"`
}

// manifest описывает файлы состояния. Замки и временные файлы — не
// состояние.
func (h *HA) manifest() ([]FileEntry, error) {
	var out []FileEntry
	for _, rel := range h.cfg.Paths {
		root := filepath.Join(h.cfg.Base, rel)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || skipReplica(d.Name()) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			relPath, err := filepath.Rel(h.cfg.Base, path)
			if err != nil {
				return err
			}
			out = append(out, FileEntry{Path: filepath.ToSlash(relPath), Size: int64(len(data)), Sum: hex.EncodeToString(sum[:])})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func skipReplica(name string) bool {
	return strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, ".ochan-tmp-")
}

// replicated — путь входит в реплицируемое и не выходит за рабочий каталог.
func (h *HA) replicated(rel string) bool {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return false
	}
	for _, p := range h.cfg.Paths {
		p = filepath.Clean(p)
		if clean == p || strings.HasPrefix(clean, p+string(filepath.Separator)) {
			return !skipReplica(filepath.Base(clean))
		}
	}
	return false
}

// pull забирает у primary изменившиеся файлы и убирает исчезнувшие.
func (h *HA) pull(ctx context.Context) error {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()

	var m Manifest
	if err := h.peerJSON(ctx, http.MethodGet, "/v1/ha/manifest", nil, &m); err != nil {
		return fmt.Errorf("опись primary: %w", err)
	}
	me := h.Status()
	if m.Epoch < me.Epoch {
		return fmt.Errorf("узел %s отдаёт опись эпохи %d, а у этого узла эпоха %d: копию от него не беру", m.Node, m.Epoch, me.Epoch)
	}
	local, err := h.manifest()
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, f := range local {
		have[f.Path] = f.Sum
	}
	want := map[string]bool{}
	for _, f := range m.Files {
		if !h.replicated(f.Path) {
			return fmt.Errorf("опись primary называет %q вне реплицируемого — копию не беру", f.Path)
		}
		want[f.Path] = true
		if have[f.Path] == f.Sum {
			continue
		}
		data, err := h.peerFile(ctx, f.Path)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.Sum {
			// Файл поменялся между описью и чтением: возьмём в следующий раз.
			continue
		}
		if err := writeAtomic(filepath.Join(h.cfg.Base, filepath.FromSlash(f.Path)), data, 0o600); err != nil {
			return err
		}
	}
	for _, f := range local {
		if !want[f.Path] {
			_ = os.Remove(filepath.Join(h.cfg.Base, filepath.FromSlash(f.Path)))
		}
	}
	if m.Epoch > me.Epoch {
		h.mu.Lock()
		st := h.state
		h.mu.Unlock()
		st.Epoch = m.Epoch
		if err := SaveHAState(h.cfg.StatePath, &st); err != nil {
			return err
		}
		_ = h.reload(true)
	}
	h.mu.Lock()
	h.lastSync = time.Now()
	h.mu.Unlock()
	return nil
}

// copyState кладёт копию реплицируемого состояния в dir.
func (h *HA) copyState(dir string) error {
	files, err := h.manifest()
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(h.cfg.Base, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(dir, filepath.FromSlash(f.Path)), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Второй узел
// ---------------------------------------------------------------------------

func (h *HA) peerStatus(ctx context.Context) (*HAStatus, error) {
	var st HAStatus
	if err := h.peerJSON(ctx, http.MethodGet, "/v1/ha/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// tellPeer сообщает второму узлу эпоху этого: отставший primary отступит,
// standby заберёт копию сейчас.
func (h *HA) tellPeer(ctx context.Context, epoch int64) error {
	return h.peerJSON(ctx, http.MethodPost, "/v1/ha/sync", map[string]int64{"epoch": epoch}, nil)
}

func (h *HA) peerRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if h.client == nil {
		return nil, errors.New("второй узел не задан (ha.peer)")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.scheme+"://"+h.cfg.Peer+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Pair "+h.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	return h.client.Do(req)
}

func (h *HA) peerJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := h.peerRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var p Problem
		if json.Unmarshal(data, &p) == nil && p.Error != "" {
			return errors.New(p.Error)
		}
		return fmt.Errorf("второй узел ответил %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (h *HA) peerFile(ctx context.Context, path string) ([]byte, error) {
	resp, err := h.peerRequest(ctx, http.MethodGet, "/v1/ha/file?path="+urlQuery(path), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("второй узел ответил %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// Barrier — пометка, сделанная на primary, дошла до standby. Standby
// недоступен: при RequireStandby — ошибка, иначе предупреждение.
func (h *HA) Barrier(ctx context.Context) (warning string, err error) {
	me := h.Status()
	if !me.Writable() {
		return "", fmt.Errorf("узел %s не primary (%s, эпоха %d): выполнять на нём нельзя", me.Node, me.Role, me.Epoch)
	}
	if h.client == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := h.tellPeer(ctx, me.Epoch); err != nil {
		if h.cfg.RequireStandby {
			return "", fmt.Errorf("резервный узел %s не подтвердил копию: %w (ha.require_standby)", h.cfg.Peer, err)
		}
		return fmt.Sprintf("резервный узел %s не подтвердил копию (%v): если этот узел пропадёт посреди "+
			"выполнения, резерв выполнит заявку ещё раз", h.cfg.Peer, err), nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// Обработчики
// ---------------------------------------------------------------------------

func (h *HA) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/ha/status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, h.Status())
	})
	mux.HandleFunc("GET /v1/ha/manifest", h.paired(func(w http.ResponseWriter, _ *http.Request) {
		me := h.Status()
		if me.Role != RolePrimary {
			reply(w, http.StatusConflict, Problem{Error: fmt.Sprintf("узел %s — standby: копию берут у primary", me.Node)})
			return
		}
		files, err := h.manifest()
		if err != nil {
			reply(w, http.StatusInternalServerError, Problem{Error: err.Error()})
			return
		}
		reply(w, http.StatusOK, Manifest{Node: me.Node, Epoch: me.Epoch, Files: files})
	}))
	mux.HandleFunc("GET /v1/ha/file", h.paired(func(w http.ResponseWriter, r *http.Request) {
		rel := r.URL.Query().Get("path")
		if !h.replicated(rel) {
			reply(w, http.StatusForbidden, Problem{Error: "файл вне реплицируемого"})
			return
		}
		data, err := os.ReadFile(filepath.Join(h.cfg.Base, filepath.FromSlash(rel)))
		if err != nil {
			reply(w, http.StatusNotFound, Problem{Error: "файла нет"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	mux.HandleFunc("POST /v1/ha/sync", h.paired(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Epoch int64 `json:"epoch"`
		}
		if !decode(w, r, loginBody, &req) {
			return
		}
		me := h.Status()
		switch {
		case req.Epoch < me.Epoch:
			reply(w, http.StatusConflict, Problem{Error: fmt.Sprintf("эпоха %d меньше эпохи узла %s (%d): "+
				"primary — этот узел", req.Epoch, me.Node, me.Epoch)})
			return
		case me.Role == RolePrimary && req.Epoch > me.Epoch:
			h.demote(req.Epoch, fmt.Sprintf("второй узел стал primary с эпохой %d", req.Epoch))
		case me.Role == RolePrimary:
			reply(w, http.StatusConflict, Problem{Error: "конфликт: оба узла primary одной эпохи"})
			return
		}
		if err := h.pull(r.Context()); err != nil {
			reply(w, http.StatusServiceUnavailable, Problem{Error: err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /v1/ha/barrier", h.barrierHandler)
}

// BarrierHandler — только барьер: его слушает отдельный адрес 127.0.0.1.
func (h *HA) BarrierHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ha/barrier", h.barrierHandler)
	return mux
}

func (h *HA) barrierHandler(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Barrier ")
	if !isLoopbackRemote(r.RemoteAddr) || subtle.ConstantTimeCompare([]byte(token), []byte(h.barrier)) != 1 {
		reply(w, http.StatusForbidden, Problem{Error: "барьер — только для процессов этого узла"})
		return
	}
	warning, err := h.Barrier(r.Context())
	if err != nil {
		reply(w, http.StatusServiceUnavailable, Problem{Error: err.Error()})
		return
	}
	reply(w, http.StatusOK, map[string]string{"warning": warning})
}

// paired — запрос второго узла пары: с общим секретом.
func (h *HA) paired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Pair ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.Token)) != 1 {
			reply(w, http.StatusUnauthorized, Problem{Error: "не узел этой пары"})
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ochan-tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func isLoopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func urlQuery(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
