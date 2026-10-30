package cmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/config"
	"octochan/fp"
	"octochan/i18n"
	"octochan/lang"
	"octochan/registry"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Реестры пакетов алиасов.
//
//	registry:
//	  default: corp                 # первым — он
//	  sources:
//	    corp:
//	      url: https://stash.example/scm/~ops/octochan-registry
//	      signed_by: corp-release   # индекс подписан этим ключом
//	    local:
//	      url: file:///opt/octochan/registry
//	    global:
//	      url: https://registry.example.org
//	      priority: 10              # ниже приоритет — позже спрашивают
//	  keys:
//	    corp-release: <открытый ключ ed25519 в base64>
//
// ochan спрашивает только эти источники. Пакет ставится заявкой: план —
// откуда, что и куда разместит, подпись и сумма, — затем «да» человека и
// apply --request, который поставит ровно тот архив, на который сказали
// «да». --approve — то же сразу, если роль может согласовывать.

// registrySources — источники из настроек: default первым, затем по
// priority и имени.
func registrySources() ([]registry.Source, error) {
	raw := viper.GetStringMap("registry.sources")
	def := viper.GetString("registry.default")
	if def != "" {
		if _, ok := raw[def]; !ok {
			return nil, fmt.Errorf("registry.default: источника %s нет в registry.sources", def)
		}
	}
	var out []registry.Source
	for name := range raw {
		key := "registry.sources." + name
		src := registry.Source{Name: name, URL: viper.GetString(key + ".url"),
			Kind: viper.GetString(key + ".type"), SignedBy: viper.GetString(key + ".signed_by"),
			Priority: viper.GetInt(key + ".priority")}
		if src.URL == "" {
			return nil, fmt.Errorf("registry.sources.%s: нет url", name)
		}
		if src.Kind == "" {
			src.Kind = registry.InferKind(src.URL)
		}
		if name == def {
			src.Priority = -1 << 30
		}
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// registryClient — клиент реестров: ключи из registry.keys, git-кеш в
// рабочем каталоге.
func registryClient() (*registry.Client, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	keys := map[string]ed25519.PublicKey{}
	for name, value := range viper.GetStringMapString("registry.keys") {
		text := value
		if p := strings.TrimPrefix(value, "file://"); p != value || filepath.IsAbs(value) {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("registry.keys.%s: %w", name, err)
			}
			text = string(data)
		}
		key, err := registry.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("registry.keys.%s: %w", name, err)
		}
		keys[name] = key
	}
	timeout := viper.GetDuration("defaults.http_timeout")
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &registry.Client{HTTP: &http.Client{Timeout: timeout}, Keys: keys,
		Cache: filepath.Join(fm.BaseDir(), "registry", "cache")}, nil
}

// fetched — индексы источников; failed — источники, которые не ответили.
type fetchResult struct {
	ok     []*registry.Fetched
	failed []string
}

func fetchSources(ctx context.Context, client *registry.Client, sources []registry.Source) fetchResult {
	var res fetchResult
	for i := range sources {
		f, err := client.Fetch(ctx, &sources[i])
		if err != nil {
			res.failed = append(res.failed, err.Error())
			continue
		}
		res.ok = append(res.ok, f)
	}
	return res
}

func sourceNames(sources []registry.Source) string {
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// pkgRef разбирает @pgconf и версию: «@pgconf v1.4.0» или «@pgconf@1.4.0».
func pkgRef(args []string) (string, string, error) {
	name := strings.TrimPrefix(args[0], "@")
	version := ""
	if n, v, ok := strings.Cut(name, "@"); ok {
		name, version = n, v
	}
	if len(args) > 1 {
		version = args[1]
	}
	if !packageName(name) {
		return "", "", fmt.Errorf("пакет %q: имя пакета — буквы, цифры и «_»", args[0])
	}
	if version != "" {
		clean := registry.CleanVersion(version)
		if clean == "" {
			return "", "", fmt.Errorf("версия %q: ожидалось 1.4.0 или v1.4.0", version)
		}
		version = clean
	}
	return name, version, nil
}

// ---------------------------------------------------------------------------
// Поиск
// ---------------------------------------------------------------------------

var searchCmd = &cobra.Command{
	Use:   "!!search <слово>",
	Short: "Найти пакет алиасов в реестрах",
	Long: "Ищет по имени и описанию пакета во всех источниках registry.sources —\n" +
		"и только в них. Источники одного приоритета — одним блоком.",
	Args:    cobra.ExactArgs(1),
	Example: "ochan '!!search' pgconf",
	RunE: func(cmd *cobra.Command, args []string) error {
		sources, err := registrySources()
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return i18n.Errorf("реестров нет: они задаются в config.yaml, раздел registry.sources")
		}
		client, err := registryClient()
		if err != nil {
			return err
		}
		res := fetchSources(cmd.Context(), client, sources)
		query := strings.ToLower(strings.TrimPrefix(args[0], "@"))
		found := 0
		// Блок — источники одного приоритета.
		var block []*registry.Fetched
		flush := func() {
			if len(block) == 0 {
				return
			}
			names := make([]string, len(block))
			var lines []string
			for i, f := range block {
				names[i] = f.Source.Name
				lines = append(lines, searchLines(f, query)...)
			}
			cmd.Println(i18n.Sprintf("Из %s:", joinAnd(names)))
			if len(lines) == 0 {
				cmd.Println(i18n.T("  ничего"))
			}
			for _, l := range lines {
				cmd.Println(l)
			}
			found += len(lines)
			block = nil
		}
		for _, f := range res.ok {
			if len(block) > 0 && block[0].Source.Priority != f.Source.Priority {
				flush()
			}
			block = append(block, f)
		}
		flush()
		for _, why := range res.failed {
			cmd.Printf("недоступен: %s\n", why)
		}
		if found == 0 && len(res.ok) > 0 {
			cmd.Printf("Ничего похожего на %q в %s\n", args[0], sourceNames(sources))
		}
		return nil
	},
}

func joinAnd(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " и " + names[len(names)-1]
}

// searchLines — строки поиска одного источника, новые версии первыми.
func searchLines(f *registry.Fetched, query string) []string {
	var names []string
	for name, e := range f.Index.Packages {
		if strings.Contains(strings.ToLower(name), query) || strings.Contains(strings.ToLower(e.Description), query) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type row struct{ ref, text string }
	var rows []row
	for _, name := range names {
		e := f.Index.Packages[name]
		for i, v := range e.Sorted() {
			ver := e.Versions[v]
			text := e.Description
			if ver.Description != "" {
				text = ver.Description
			}
			if i > 0 {
				text = "то же, старее"
			}
			if ver.Revoked != "" {
				text = "отозван: " + ver.Revoked
			}
			rows = append(rows, row{fmt.Sprintf("@%s v%s", name, v), text})
		}
	}
	refW, textW := 0, 0
	for _, r := range rows {
		refW, textW = max(refW, len([]rune(r.ref))), max(textW, len([]rune(r.text)))
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("  %s   %s   (%s)", pad(r.ref, refW), pad(r.text, textW), f.Source.Name)
	}
	return out
}

func pad(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-len([]rune(s))))
}

// ---------------------------------------------------------------------------
// Разрешение и план
// ---------------------------------------------------------------------------

// resolved — версия пакета из источника.
type resolved struct {
	name, version string
	ver           *registry.Version
	from          *registry.Fetched
	// installed — эта версия уже стоит: ставить нечего.
	installed bool
}

func (r *resolved) install() workspace.PackageInstall {
	return workspace.PackageInstall{Name: r.name, Version: r.version, Source: r.from.Source.Name,
		URL: r.from.Source.URL, SHA256: r.ver.SHA256, SignedBy: r.from.SignedBy}
}

// resolvePackage — первый по приоритету источник, где пакет есть.
func resolvePackage(res fetchResult, sources []registry.Source, name, version string) (*resolved, error) {
	var revokedIn []string
	for _, f := range res.ok {
		e := f.Index.Packages[name]
		if e == nil {
			continue
		}
		if version == "" {
			v, ver := e.Latest()
			if ver == nil {
				revokedIn = append(revokedIn, f.Source.Name)
				continue
			}
			return &resolved{name: name, version: v, ver: ver, from: f}, nil
		}
		for _, v := range e.Sorted() {
			if registry.Compare(v, version) != 0 {
				continue
			}
			ver := e.Versions[v]
			if ver.Revoked != "" {
				hint := ""
				if latest, lv := e.Latest(); lv != nil {
					hint = "; доступна v" + latest
				}
				return nil, i18n.Errorf("@%s v%s отозван автором (%s) — не ставится%s", name, v, ver.Revoked, hint)
			}
			if ver.PackageProtocol() > registry.Protocol {
				return nil, i18n.Errorf("@%s v%s — пакет протокола v%d, эта сборка понимает v%d", name, v,
					ver.PackageProtocol(), registry.Protocol)
			}
			return &resolved{name: name, version: v, ver: ver, from: f}, nil
		}
	}
	what := "@" + name
	if version != "" {
		what += " v" + version
	}
	msg := i18n.Sprintf("пакет %s не найден в настроенных источниках (%s); других ochan не спрашивает — сеть вне них не использовалась",
		what, sourceNames(sources))
	if len(revokedIn) > 0 {
		msg = i18n.Sprintf("все версии @%s в %s отозваны", name, strings.Join(revokedIn, ", "))
	}
	if len(res.failed) > 0 {
		msg += "\n" + i18n.Sprintf("не ответили: %s", strings.Join(res.failed, "; "))
	}
	return nil, fmt.Errorf("%s", msg)
}

// installedPackages — версии установленных пакетов алиасов.
func installedPackages() map[string]string {
	lib, _ := aliases()
	out := map[string]string{}
	for _, info := range lib.List() {
		if info.Module == "" {
			continue
		}
		if pkg, ok := lib.Package(info.Module); ok {
			out[info.Module] = pkg.Version
		} else if _, seen := out[info.Module]; !seen {
			out[info.Module] = ""
		}
	}
	return out
}

// planInstall — пакет и зависимости, которых нет: зависимости первыми.
func planInstall(res fetchResult, sources []registry.Source, root *resolved) ([]*resolved, error) {
	have := installedPackages()
	var plan []*resolved
	seen := map[string]bool{}
	var visit func(r *resolved, chain []string) error
	visit = func(r *resolved, chain []string) error {
		if seen[r.name] {
			return nil
		}
		for _, c := range chain {
			if c == r.name {
				return fmt.Errorf("зависимости по кругу: %s → %s", strings.Join(chain, " → "), r.name)
			}
		}
		for _, text := range r.ver.Requires {
			req, err := registry.ParseRequire(text)
			if err != nil {
				return fmt.Errorf("@%s v%s: %w", r.name, r.version, err)
			}
			if v, ok := have[req.Name]; ok && req.Satisfied(v) {
				continue
			}
			dep, err := resolveRequire(res, sources, req)
			if err != nil {
				return fmt.Errorf("@%s v%s требует %s: %w", r.name, r.version, req, err)
			}
			if err := visit(dep, append(chain, r.name)); err != nil {
				return err
			}
		}
		seen[r.name] = true
		r.installed = have[r.name] == r.version && r.version != ""
		plan = append(plan, r)
		return nil
	}
	return plan, visit(root, nil)
}

// resolveRequire — новейшая подходящая версия зависимости.
func resolveRequire(res fetchResult, sources []registry.Source, req registry.Require) (*resolved, error) {
	for _, f := range res.ok {
		e := f.Index.Packages[req.Name]
		if e == nil {
			continue
		}
		for _, v := range e.Sorted() {
			if ver := e.Versions[v]; ver.Revoked == "" && req.Satisfied(v) {
				return &resolved{name: req.Name, version: v, ver: ver, from: f}, nil
			}
		}
	}
	return nil, fmt.Errorf("подходящей версии нет в %s", sourceNames(sources))
}

// printInstallPlan — план установки.
func printInstallPlan(cmd *cobra.Command, plan []*resolved, modulesDir string) {
	cmd.Println(i18n.T("План установки:"))
	for _, r := range plan {
		cmd.Printf("  @%s v%s\n", r.name, r.version)
		if r.installed {
			cmd.Println(i18n.T("    уже установлен — пропускается"))
			continue
		}
		cmd.Printf("    источник:    %s (%s)\n", r.from.Source.Name, r.from.Source.URL)
		cmd.Printf("    разместит:   %s%c, файлов: %d\n", filepath.Join(modulesDir, r.name), filepath.Separator, r.ver.Files)
		cmd.Printf("    режимы:      %s\n", noneOrList(r.ver.Modes, "не названы"))
		cmd.Printf("    зависимости: %s\n", noneOrList(r.ver.Requires, "ничего"))
		if r.from.SignedBy != "" {
			cmd.Printf("    подпись:     %s ✓ (индекс)\n", r.from.SignedBy)
		} else {
			cmd.Printf("    подпись:     %s\n", i18n.T("нет — у источника нет signed_by"))
		}
		cmd.Printf("    sha256:      %s (сверяется при установке)\n", registry.Short(r.ver.SHA256))
		cmd.Printf("    протокол:    v%d (поддерживается)\n", r.ver.PackageProtocol())
	}
}

func noneOrList(list []string, none string) string {
	if len(list) == 0 {
		return none
	}
	return strings.Join(list, ", ")
}

// ---------------------------------------------------------------------------
// Установка
// ---------------------------------------------------------------------------

var (
	rinPlan    bool
	rinApprove bool
)

var rinCmd = &cobra.Command{
	Use:   "!!rin <@пакет> [версия]",
	Short: "Поставить пакет алиасов из реестра — с планом и согласованием",
	Long: "Ищет пакет в registry.sources по приоритету, строит план установки —\n" +
		"источник, что и куда разместит, режимы, зависимости, подпись индекса,\n" +
		"sha256 архива — и оформляет заявку. После ochan approve <id> пакет\n" +
		"ставит ochan apply --request <id>: ровно тот архив, сумму которого\n" +
		"согласовали. Отозванная автором версия не ставится.\n\n" +
		"--plan — только показать план; --approve — поставить сразу, если роль\n" +
		"может согласовывать.",
	Args: cobra.RangeArgs(1, 2),
	Example: "ochan '!!rin' @pgconf --plan\n" +
		"ochan '!!rin' @pgconf v1.4.0\n" +
		"ochan '!!rin' @pgconf --approve",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, version, err := pkgRef(args)
		if err != nil {
			return err
		}
		sources, err := registrySources()
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return i18n.Errorf("реестров нет: они задаются в config.yaml, раздел registry.sources")
		}
		client, err := registryClient()
		if err != nil {
			return err
		}
		res := fetchSources(cmd.Context(), client, sources)
		root, err := resolvePackage(res, sources, name, version)
		if err != nil {
			return err
		}
		plan, err := planInstall(res, sources, root)
		if err != nil {
			return err
		}
		fm, err := mustFileManager()
		if err != nil {
			return err
		}
		printInstallPlan(cmd, plan, fm.ModulesDir())
		todo := pendingInstalls(plan)
		if len(todo) == 0 {
			cmd.Println(i18n.T("Ставить нечего: всё уже установлено"))
			return nil
		}
		switch {
		case rinPlan:
			cmd.Println(i18n.T("  требует одобрения: да (--approve или ochan approve <id> после заявки)"))
			return nil
		case rinApprove:
			if currentRole != nil && !currentRole.Allows("approve") {
				return refuse(cmd, i18n.Errorf("роль %s не может согласовывать: --approve недоступен — "+
					"оформите заявку (без --approve), её согласует роль с правом approve", currentRole.Name))
			}
			return installAll(cmd, client, todo)
		}
		return requestInstall(cmd, todo)
	},
}

func pendingInstalls(plan []*resolved) []*resolved {
	var out []*resolved
	for _, r := range plan {
		if !r.installed {
			out = append(out, r)
		}
	}
	return out
}

// requestInstall оформляет заявку на установку.
func requestInstall(cmd *cobra.Command, todo []*resolved) error {
	store, err := requestStore()
	if err != nil {
		return err
	}
	plan := &config.Plan{Command: "!!rin", Source: "реестр"}
	var names []string
	request := &workspace.Request{Source: "реестр " + todo[len(todo)-1].from.Source.Name}
	for _, r := range todo {
		plan.Add(config.Change{Kind: config.ChangeTask, Target: "@" + r.name,
			Name:   "установить v" + r.version,
			Detail: fmt.Sprintf("из %s, sha256 %s", r.from.Source.Name, registry.Short(r.ver.SHA256))})
		request.Packages = append(request.Packages, r.install())
		names = append(names, fmt.Sprintf("@%s v%s", r.name, r.version))
	}
	request.Plan = plan
	request.Summary = "установка " + strings.Join(names, ", ")
	stampRequest(request)
	if err := store.Create(request); err != nil {
		return err
	}
	cmd.Printf("Заявка %s создана (ждёт решения): %s\n", request.ID, request.Summary)
	cmd.Printf("Согласовать: ochan approve %s; поставить: ochan apply --request %s\n", request.ID, request.ID)
	return nil
}

// unpacked — архив, скачанный, сверенный с суммой и распакованный.
type unpacked struct {
	r     *resolved
	dir   string
	files []string
}

// prepareAll скачивает и проверяет все архивы до того, как что-то
// поставлено: подменённый архив — отказ до выполнения, а не сбой на
// полпути. Вызывающий удаляет tmp.
func prepareAll(cmd *cobra.Command, client *registry.Client, todo []*resolved) ([]unpacked, string, error) {
	tmp, err := os.MkdirTemp("", "ochan-rin-")
	if err != nil {
		return nil, "", err
	}
	var out []unpacked
	for _, r := range todo {
		data, err := client.Archive(cmd.Context(), r.from, r.ver)
		if err != nil {
			return nil, tmp, err
		}
		dir := filepath.Join(tmp, r.name)
		files, err := registry.Unpack(data, dir)
		if err != nil {
			return nil, tmp, fmt.Errorf("@%s v%s: %w", r.name, r.version, err)
		}
		pkg, err := lang.LoadManifest(r.name, dir)
		switch {
		case err != nil:
			return nil, tmp, fmt.Errorf("@%s v%s: %w", r.name, r.version, err)
		case pkg == nil:
			return nil, tmp, fmt.Errorf("@%s v%s: в архиве нет %s — версия пакета неизвестна", r.name, r.version, lang.ManifestName)
		case registry.Compare(pkg.Version, r.version) != 0:
			return nil, tmp, fmt.Errorf("@%s: индекс называет версию %s, а package.yaml в архиве — %s: индекс и пакет расходятся",
				r.name, r.version, pkg.Version)
		}
		out = append(out, unpacked{r: r, dir: dir, files: files})
	}
	return out, tmp, nil
}

// installAll ставит пакеты по порядку: зависимости первыми.
func installAll(cmd *cobra.Command, client *registry.Client, todo []*resolved) error {
	ready, tmp, err := prepareAll(cmd, client, todo)
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return err
	}
	return installPrepared(cmd, ready)
}

func installPrepared(cmd *cobra.Command, ready []unpacked) error {
	for _, u := range ready {
		if err := installResolved(cmd, u); err != nil {
			return err
		}
	}
	return nil
}

// installResolved ставит распакованный пакет тем же путём, что !!install
// каталога.
func installResolved(cmd *cobra.Command, u unpacked) error {
	r, dir := u.r, u.dir
	cmd.Printf("Установлен @%s v%s из %s: sha256 %s ✓, файлов %d\n", r.name, r.version, r.from.Source.Name,
		registry.Short(r.ver.SHA256), len(u.files))
	if err := installDir(cmd, dir); err != nil {
		return err
	}
	lib, _ := aliases()
	var names []string
	for _, info := range lib.List() {
		if info.Module == r.name {
			names = append(names, info.Name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		cmd.Printf("Алиасы: %s\n", strings.Join(names, ", "))
	}
	target := r.name
	if fm, err := mustFileManager(); err == nil {
		target = filepath.Join(fm.ModulesDir(), r.name)
	}
	record(cmd, &workspace.Entry{Kind: workspace.EntryApply, Command: "!!rin @" + r.name,
		Source: "реестр " + r.from.Source.Name, Target: target,
		Summary: fmt.Sprintf("установлен @%s v%s, sha256 %s", r.name, r.version, r.ver.SHA256)})
	return nil
}

// applyPackageRequest ставит пакеты согласованной заявки: те же версии из
// тех же источников и с той же суммой, иначе — отказ.
func applyPackageRequest(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) error {
	sources, err := registrySources()
	if err != nil {
		return err
	}
	client, err := registryClient()
	if err != nil {
		return err
	}
	res := fetchSources(cmd.Context(), client, sources)
	var todo []*resolved
	for _, p := range request.Packages {
		var from *registry.Fetched
		for _, f := range res.ok {
			if f.Source.Name == p.Source {
				from = f
			}
		}
		if from == nil {
			return fmt.Errorf("заявка %s: источник %s не ответил или пропал из настроек: %s", request.ID, p.Source,
				strings.Join(res.failed, "; "))
		}
		e := from.Index.Packages[p.Name]
		var ver *registry.Version
		if e != nil {
			ver = e.Versions[p.Version]
		}
		switch {
		case ver == nil:
			return fmt.Errorf("заявка %s: @%s v%s в %s больше нет", request.ID, p.Name, p.Version, p.Source)
		case ver.Revoked != "":
			return fmt.Errorf("заявка %s: @%s v%s отозван автором после согласования (%s) — не ставится",
				request.ID, p.Name, p.Version, ver.Revoked)
		case !strings.EqualFold(ver.SHA256, p.SHA256):
			return fmt.Errorf("заявка %s: sha256 @%s v%s в %s теперь %s, а согласовали %s — поставился бы не тот архив; оформите новую заявку",
				request.ID, p.Name, p.Version, p.Source, registry.Short(ver.SHA256), registry.Short(p.SHA256))
		case p.SignedBy != "" && from.SignedBy != p.SignedBy:
			return fmt.Errorf("заявка %s: индекс %s больше не подписан ключом %s", request.ID, p.Source, p.SignedBy)
		}
		todo = append(todo, &resolved{name: p.Name, version: p.Version, ver: ver, from: from})
	}
	ready, tmp, err := prepareAll(cmd, client, todo)
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	if err != nil {
		return fmt.Errorf("заявка %s: %w; выполнение не начиналось", request.ID, err)
	}
	if err := beginRun(cmd, store, request); err != nil {
		return err
	}
	err = installPrepared(cmd, ready)
	var refs []string
	for _, p := range request.Packages {
		refs = append(refs, "@"+p.Name+" v"+p.Version)
	}
	markOutcome(cmd, store, request, refs, err)
	if err != nil {
		return err
	}
	cmd.Printf("Заявка %s выполнена: %s\n", request.ID, request.Summary)
	return nil
}

// ---------------------------------------------------------------------------
// Свой реестр
// ---------------------------------------------------------------------------

var (
	publishRegistry string
	publishKey      string
	revokeReason    string
)

var registryCmd = &cobra.Command{
	Use:   "!!registry",
	Short: "Реестры пакетов: источники, ключи подписи, публикация, отзыв",
	Long: "Без подкоманды — источники registry.sources по приоритету и ключи\n" +
		"registry.keys. Свой реестр — каталог: keygen создаёт ключ подписи, publish\n" +
		"кладёт пакет и подписывает индекс, revoke отзывает версию. Каталог\n" +
		"отдают по HTTP или коммитят в git.",
	Args: cobra.NoArgs,
	Example: "ochan '!!registry'\n" +
		"ochan '!!registry' keygen corp-release\n" +
		"ochan '!!registry' publish ./pgconf --registry /srv/registry --key corp-release.key\n" +
		"ochan '!!registry' revoke @pgconf v1.3.0 --registry /srv/registry --key corp-release.key --reason \"CVE-2026-1234\"",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sources, err := registrySources()
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			cmd.Println(i18n.T("Реестров нет: они задаются в config.yaml, раздел registry.sources"))
			return nil
		}
		def := viper.GetString("registry.default")
		for _, s := range sources {
			mark, prio := " ", fmt.Sprint(s.Priority)
			if s.Name == def {
				mark, prio = "*", "default"
			}
			signed := i18n.T("без подписи")
			if s.SignedBy != "" {
				signed = i18n.Sprintf("подпись %s", s.SignedBy)
			}
			cmd.Printf("%s %-12s %-5s %s\n", mark, s.Name, s.Kind, s.URL)
			cmd.Printf("  %-12s       %s, приоритет %s\n", "", signed, prio)
		}
		cmd.Println(i18n.T("\n* — registry.default: его спрашивают первым"))
		return nil
	},
}

var registryKeygenCmd = &cobra.Command{
	Use:   "keygen <имя>",
	Short: "Создать ключ подписи реестра ed25519",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		keyFile, pubFile := args[0]+".key", args[0]+".pub"
		for _, f := range []string{keyFile, pubFile} {
			if _, err := os.Stat(f); err == nil {
				return fmt.Errorf("%s уже есть: ключ не перезаписывается", f)
			}
		}
		if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
			return err
		}
		pubText := base64.StdEncoding.EncodeToString(pub)
		if err := os.WriteFile(pubFile, []byte(pubText+"\n"), 0o644); err != nil {
			return err
		}
		cmd.Printf("Закрытый ключ: %s (права 0600) — им подписывают индекс, из реестра его не выкладывают\n", keyFile)
		cmd.Printf("Открытый ключ: %s\n", pubFile)
		cmd.Printf("В config.yaml тех, кто ставит:\n  registry:\n    keys:\n      %s: %s\n", args[0], pubText)
		return nil
	},
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return registry.ParsePrivateKey(string(data))
}

var registryPublishCmd = &cobra.Command{
	Use:   "publish <каталог пакета> --registry <каталог реестра>",
	Short: "Опубликовать пакет в реестр-каталог",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		name := filepath.Base(dir)
		if !packageName(name) {
			return fmt.Errorf("пакет %q: имя пакета — имя каталога, буквы, цифры и «_»", name)
		}
		check := lang.NewRegistry()
		if err := check.LoadDir(name, dir); err != nil {
			return fmt.Errorf("пакет %s не опубликован: %w", name, err)
		}
		pkg, err := lang.LoadManifest(name, dir)
		if err != nil {
			return err
		}
		if pkg == nil {
			return fmt.Errorf("пакет %s без %s: в реестре у пакета есть версия", name, lang.ManifestName)
		}
		if _, err := os.Stat(filepath.Join(dir, fp.LexiconName)); err == nil {
			if _, err := fp.LoadLexicon(filepath.Join(dir, fp.LexiconName)); err != nil {
				return fmt.Errorf("пакет %s не опубликован: %w", name, err)
			}
		}
		key, err := loadPrivateKey(publishKey)
		if err != nil {
			return fmt.Errorf("--key: %w", err)
		}
		data, files, err := registry.Pack(dir)
		if err != nil {
			return err
		}
		file, err := registry.Publish(publishRegistry, name, pkg.Version,
			registry.Meta{Description: pkg.Description, Modes: pkg.Modes, Requires: pkg.Requires},
			data, len(files), key, time.Now())
		if err != nil {
			return err
		}
		cmd.Printf("Опубликован @%s v%s: %s, файлов %d, sha256 %s\n", name, pkg.Version,
			filepath.Join(publishRegistry, file), len(files), registry.Short(registry.Sum(data)))
		if key != nil {
			cmd.Println(i18n.T("Индекс подписан заново"))
		} else {
			cmd.Println(i18n.T("Индекс без подписи: источнику с signed_by такой реестр не годится"))
		}
		return nil
	},
}

var registryRevokeCmd = &cobra.Command{
	Use:   "revoke <@пакет> <версия> --registry <каталог реестра> --reason <почему>",
	Short: "Отозвать версию пакета в реестре-каталоге",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, version, err := pkgRef(args)
		if err != nil {
			return err
		}
		key, err := loadPrivateKey(publishKey)
		if err != nil {
			return fmt.Errorf("--key: %w", err)
		}
		if err := registry.Revoke(publishRegistry, name, version, revokeReason, key); err != nil {
			return err
		}
		cmd.Printf("@%s v%s отозван: %s\n", name, version, revokeReason)
		return nil
	},
}

func init() {
	rinCmd.Flags().BoolVar(&rinPlan, "plan", false, "только показать план установки")
	rinCmd.Flags().BoolVar(&rinApprove, "approve", false, "поставить сразу, без заявки: роли нужно право approve")
	for _, c := range []*cobra.Command{registryPublishCmd, registryRevokeCmd} {
		c.Flags().StringVar(&publishRegistry, "registry", "", "каталог реестра")
		c.Flags().StringVar(&publishKey, "key", "", "закрытый ключ подписи индекса (keygen)")
		_ = c.MarkFlagRequired("registry")
	}
	registryRevokeCmd.Flags().StringVar(&revokeReason, "reason", "", "причина отзыва: её увидят все")
	registryCmd.AddCommand(registryKeygenCmd, registryPublishCmd, registryRevokeCmd)
	rootCmd.AddCommand(searchCmd, rinCmd, registryCmd)
}
