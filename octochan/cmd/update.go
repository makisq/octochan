package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"ochan/core/i18n"
	"ochan/core/workspace"

	"github.com/spf13/cobra"
)

// Обновление ochan: новая программа встаёт на место установленной, а
// рабочий каталог — модули, токены, config.yaml, роли, заявки — остаётся
// как был. Прежние программы и копия config.yaml сжимаются в архив рядом
// (ochan-versions/): удалить его или оставить — решает пользователь. Откат
// — из архива, программы и конфиг, а текущие перед ним тоже уходят в
// архив: откат можно откатить. Новая версия, которой нужен другой вид
// конфига, переносит значения сама (configRenames) и прежних строк не
// удаляет — прежняя версия после отката читает тот же файл.

// versionsDir — каталог архивов прежних версий рядом с программой.
const versionsDir = "ochan-versions"

var (
	updateRollback bool
	updateList     bool
)

var updateCmd = &cobra.Command{
	Use:   "update <новый ochan или папка с ним> | --rollback [архив] | --list",
	Short: "Обновить ochan, не трогая модули, токены и настройки",
	Long: "Ставит новую программу ochan (и ochan-mcp, если она рядом) на место той,\n" +
		"что запущена. Рабочий каталог — модули, токены, config.yaml, роли,\n" +
		"заявки, история — не трогается. Прежние программы сжимаются в архив\n" +
		"ochan-versions/ochan-<версия>-<время> рядом с программой: удалить его\n" +
		"или оставить — решать вам. После замены новая программа проверяется\n" +
		"(--version) и загружает модули; не запустилась — прежняя возвращается\n" +
		"сама.\n\n" +
		"--rollback — вернуть программы из последнего архива (или из указанного);\n" +
		"текущие перед этим тоже уходят в архив. --list — архивы прежних версий.\n\n" +
		"Новая версия скачана в другую папку — можно и так: запустить её\n" +
		"ochan setup, он найдёт установленный ochan и предложит обновить.",
	Example: "ochan update ~/Downloads/octochan-0.3.1\n" +
		"ochan update ./build/ochan\n" +
		"ochan update --rollback\n" +
		"ochan update --list",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bin, err := workspace.ProgramDir()
		if err != nil {
			return err
		}
		switch {
		case updateList:
			return listVersions(cmd, bin)
		case updateRollback:
			archive := ""
			if len(args) == 1 {
				archive = args[0]
			}
			return rollbackPrograms(cmd.OutOrStdout(), bin, archive)
		case len(args) == 0:
			return i18n.Errorf("укажите новый ochan или папку с ним: ochan update <путь>; откат — ochan update --rollback")
		}
		return upgradePrograms(cmd.OutOrStdout(), args[0], bin)
	},
}

// programExt — расширение программ на этой системе.
func programExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// newPrograms — программы новой версии по пути: файл ochan или папка с
// ним; ochan-mcp — если лежит рядом.
func newPrograms(path string) (dir string, names []string, err error) {
	path, err = filepath.Abs(expandHome(path))
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, i18n.Errorf("новой программы нет: %v", err)
	}
	main := "ochan" + programExt()
	if info.IsDir() {
		dir = path
		if _, err := os.Stat(filepath.Join(dir, main)); err != nil {
			return "", nil, i18n.Errorf("в папке %s нет %s", dir, main)
		}
	} else {
		dir, main = filepath.Dir(path), filepath.Base(path)
	}
	names = []string{main}
	if _, err := os.Stat(filepath.Join(dir, "ochan-mcp"+programExt())); err == nil {
		names = append(names, "ochan-mcp"+programExt())
	}
	return dir, names, nil
}

// programVersion — версия программы ochan: что печатает её --version.
// Не ochan — ошибка.
func programVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	text := strings.TrimSpace(string(out))
	version, ok := strings.CutPrefix(text, "ochan ")
	if err != nil || !ok || version == "" || strings.ContainsAny(version, "\n ") {
		return "", i18n.Errorf("%s — не программа ochan (--version: %q)", path, text)
	}
	return version, nil
}

// fileSum — SHA-256 файла.
func fileSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// installedPrograms — программы установки в bin: ochan и, если есть,
// ochan-mcp.
func installedPrograms(bin string) []string {
	names := []string{"ochan" + programExt()}
	if _, err := os.Stat(filepath.Join(bin, "ochan-mcp"+programExt())); err == nil {
		names = append(names, "ochan-mcp"+programExt())
	}
	return names
}

// upgradePrograms ставит новую версию из src в каталог программ bin.
func upgradePrograms(out io.Writer, src, bin string) error {
	srcDir, srcNames, err := newPrograms(src)
	if err != nil {
		return err
	}
	main := filepath.Join(bin, "ochan"+programExt())
	if _, err := os.Stat(main); err != nil {
		return i18n.Errorf("в %s нет ochan — это установка, а не обновление: ochan setup", bin)
	}
	newMain := filepath.Join(srcDir, srcNames[0])
	if same, _ := sameFile(newMain, main); same {
		return i18n.Errorf("%s — и есть установленная программа: укажите новую версию", newMain)
	}
	newVersion, err := programVersion(newMain)
	if err != nil {
		return err
	}
	oldVersion, err := programVersion(main)
	if err != nil {
		oldVersion = "?"
	}
	if sameBuild(srcDir, srcNames, bin) {
		fmt.Fprintln(out, i18n.Sprintf("Установлена эта же сборка ochan %s — обновлять нечего", oldVersion))
		return nil
	}

	config := installConfigPath(bin)
	before, _ := os.ReadFile(config)
	archive, err := archivePrograms(bin, oldVersion, config)
	if err != nil {
		return i18n.Errorf("прежняя версия не сохранена, обновление не начато: %v", err)
	}
	for i, name := range srcNames {
		target := name
		if i == 0 {
			target = "ochan" + programExt()
		}
		if err := installProgram(filepath.Join(srcDir, name), filepath.Join(bin, target)); err != nil {
			restoreErr := extractPrograms(archive, bin, "")
			return i18n.Errorf("%s не заменён: %v; прежняя версия возвращена из %s%s", target, err, archive, failure(restoreErr))
		}
	}
	got, err := programVersion(main)
	if err != nil || got != newVersion {
		restoreErr := extractPrograms(archive, bin, "")
		return i18n.Errorf("новая программа не запустилась (%v) — прежняя ochan %s возвращена из %s%s", err, oldVersion, archive,
			failure(restoreErr))
	}

	fmt.Fprintln(out, i18n.Sprintf("ochan обновлён: %s → %s (%s)", oldVersion, newVersion, bin))
	fmt.Fprintln(out, i18n.Sprintf("  прежняя версия — архив %s: удалить или оставить — решать вам", archive))
	fmt.Fprintln(out, i18n.T("  рабочий каталог не тронут: модули, токены, роли, заявки — как были; config.yaml — копия в архиве"))
	reportModules(out, main)
	reportConfig(out, config, before, archive)
	if changelog := findChangelog(srcDir); changelog != "" {
		fmt.Fprintln(out, i18n.Sprintf("  что нового — %s", changelog))
	}
	fmt.Fprintln(out, i18n.T("  открытые оболочки ochan работают на прежней версии до перезапуска"))
	fmt.Fprintln(out, i18n.T("  вернуть прежнюю — ochan update --rollback"))
	return nil
}

// sameBuild — в bin уже стоят те же программы, что в srcDir: ochan и,
// если она есть в новой версии, ochan-mcp.
func sameBuild(srcDir string, srcNames []string, bin string) bool {
	for i, name := range srcNames {
		target := name
		if i == 0 {
			target = "ochan" + programExt()
		}
		a, err1 := fileSum(filepath.Join(srcDir, name))
		b, err2 := fileSum(filepath.Join(bin, target))
		if err1 != nil || err2 != nil || !bytes.Equal(a, b) {
			return false
		}
	}
	return true
}

// failure — хвост сообщения об ошибке восстановления.
func failure(err error) string {
	if err == nil {
		return ""
	}
	return i18n.Sprintf(" (и восстановить не удалось: %v — распакуйте архив сами)", err)
}

// reportModules — новая программа загружает установленные модули: что
// она о них говорит. Модуль не загрузился — его видно здесь, а не при
// первой команде.
func reportModules(out io.Writer, program string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	data, err := exec.CommandContext(ctx, program, "modules").CombinedOutput()
	text := strings.TrimSpace(string(data))
	if err != nil {
		fmt.Fprintln(out, i18n.Sprintf("  внимание: ochan modules новой версии: %v", err))
	}
	if text == "" {
		return
	}
	fmt.Fprintln(out, i18n.T("  модули в новой версии:"))
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintln(out, "    "+line)
	}
}

// findChangelog — CHANGELOG.md рядом с новой программой или на уровень выше
// (папка исходников, распакованный архив релиза).
func findChangelog(dir string) string {
	for _, d := range []string{dir, filepath.Dir(dir)} {
		path := filepath.Join(d, "CHANGELOG.md")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// archiveName — имя архива версии: ochan-0.3.0-alpha-20261123-153000.tar.gz
// (на Windows — .zip).
func archiveName(version string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, version)
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "ochan-" + clean + "-" + time.Now().Format("20060102-150405") + ext
}

// archivePrograms сжимает программы установки и копию config.yaml (если
// есть) в ochan-versions/ и возвращает путь архива. Секретов в config.yaml
// нет: токены — в tokens/, они в архив не попадают.
func archivePrograms(bin, version, config string) (string, error) {
	dir := filepath.Join(bin, versionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := archiveName(version)
	path := filepath.Join(dir, name)
	// Две архивации в одну секунду (откат сразу после обновления) — номер.
	ext := ".tar.gz"
	if strings.HasSuffix(name, ".zip") {
		ext = ".zip"
	}
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
	}
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if err := writeArchive(f, bin, installedPrograms(bin), config, strings.HasSuffix(path, ".zip")); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func writeArchive(w io.Writer, bin string, names []string, config string, asZip bool) error {
	type member struct{ path, name string }
	var members []member
	for _, name := range names {
		members = append(members, member{filepath.Join(bin, name), name})
	}
	if config != "" {
		if _, err := os.Stat(config); err == nil {
			members = append(members, member{config, configMember})
		}
	}
	if asZip {
		zw := zip.NewWriter(w)
		for _, m := range members {
			if err := addZip(zw, m.path, m.name); err != nil {
				return err
			}
		}
		return zw.Close()
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		if err := addTar(tw, m.path, m.name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// configMember — копия config.yaml в архиве версии.
const configMember = "config.yaml"

func addTar(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	mode := int64(0o755)
	if name == configMember {
		mode = 0o600
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addZip(zw *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name, header.Method = name, zip.Deflate
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// archiveMember — имя программы в архиве версии: только ochan и
// ochan-mcp, без путей — архив не пишет никуда, кроме каталога программ.
func archiveMember(name string) (string, bool) {
	switch name {
	case "ochan", "ochan.exe", "ochan-mcp", "ochan-mcp.exe", configMember:
		return name, true
	}
	return "", false
}

// extractPrograms возвращает программы из архива версии в bin, а копию
// config.yaml — в config (пусто — конфиг не трогается).
func extractPrograms(archive, bin, config string) error {
	files, err := readArchive(archive)
	if err != nil {
		return err
	}
	if _, ok := files["ochan"+programExt()]; !ok {
		return i18n.Errorf("в архиве %s нет ochan%s", archive, programExt())
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == configMember {
			if config != "" {
				if err := writeFileAtomic(config, files[name], 0o600); err != nil {
					return err
				}
			}
			continue
		}
		tmp := filepath.Join(bin, "."+name+".restore")
		if err := os.WriteFile(tmp, files[name], 0o755); err != nil {
			return err
		}
		err := installProgram(tmp, filepath.Join(bin, name))
		os.Remove(tmp)
		if err != nil {
			return err
		}
	}
	return nil
}

// maxProgramSize — предел программы в архиве: защита от «архива-бомбы».
const maxProgramSize = 1 << 30

func readArchive(path string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if strings.HasSuffix(path, ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		for _, f := range zr.File {
			name, ok := archiveMember(f.Name)
			if !ok {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(rc, maxProgramSize))
			rc.Close()
			if err != nil {
				return nil, err
			}
			files[name] = data
		}
		return files, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, i18n.Errorf("%s — не архив версии ochan: %v", path, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name, ok := archiveMember(h.Name)
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxProgramSize))
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
	return files, nil
}

// versionArchives — архивы прежних версий, новые первыми.
func versionArchives(bin string) []string {
	entries, err := os.ReadDir(filepath.Join(bin, versionsDir))
	if err != nil {
		return nil
	}
	type item struct {
		path string
		when time.Time
	}
	var items []item
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "ochan-") || !(strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(bin, versionsDir, name), info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].when.Equal(items[j].when) {
			return items[i].when.After(items[j].when)
		}
		return items[i].path > items[j].path
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.path
	}
	return out
}

func listVersions(cmd *cobra.Command, bin string) error {
	archives := versionArchives(bin)
	if len(archives) == 0 {
		cmd.Println(i18n.Sprintf("Архивов прежних версий нет (%s)", filepath.Join(bin, versionsDir)))
		return nil
	}
	cmd.Println(i18n.Sprintf("Прежние версии (%s), новые сверху:", filepath.Join(bin, versionsDir)))
	for _, path := range archives {
		size := int64(0)
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		cmd.Printf("  %s  %d КиБ\n", filepath.Base(path), size>>10)
	}
	cmd.Println(i18n.T("Вернуть — ochan update --rollback [архив]; удалить ненужные — просто удалите файлы"))
	return nil
}

// rollbackPrograms возвращает программы из архива (пусто — последнего);
// текущие перед этим уходят в архив.
func rollbackPrograms(out io.Writer, bin, archive string) error {
	if archive == "" {
		archives := versionArchives(bin)
		if len(archives) == 0 {
			return i18n.Errorf("архивов прежних версий нет в %s: откатывать не к чему", filepath.Join(bin, versionsDir))
		}
		archive = archives[0]
	} else if !filepath.IsAbs(archive) && !strings.ContainsRune(archive, os.PathSeparator) {
		if _, err := os.Stat(archive); err != nil {
			archive = filepath.Join(bin, versionsDir, archive)
		}
	}
	if _, err := readArchive(archive); err != nil {
		return i18n.Errorf("архив %s не прочитать: %v", archive, err)
	}
	main := filepath.Join(bin, "ochan"+programExt())
	current, err := programVersion(main)
	if err != nil {
		current = "?"
	}
	config := installConfigPath(bin)
	saved, err := archivePrograms(bin, current, config)
	if err != nil {
		return i18n.Errorf("текущая версия не сохранена, откат не начат: %v", err)
	}
	// Конфиг — тот, с которым работала возвращаемая версия: новая могла
	// дописать в него то, чего прежняя не знает. Текущий — в архиве saved.
	if err := extractPrograms(archive, bin, config); err != nil {
		return i18n.Errorf("откат не удался: %v; текущая версия — в %s", err, saved)
	}
	restored, err := programVersion(main)
	if err != nil {
		restored = "?"
	}
	fmt.Fprintln(out, i18n.Sprintf("ochan возвращён: %s → %s (%s)", current, restored, bin))
	fmt.Fprintln(out, i18n.Sprintf("  версия %s — архив %s: вернуть её — ochan update --rollback", current, saved))
	if files, err := readArchive(archive); err == nil {
		if _, ok := files[configMember]; ok {
			fmt.Fprintln(out, i18n.Sprintf("  config.yaml — как был у версии %s; нынешний — в %s", restored, saved))
		}
	}
	fmt.Fprintln(out, i18n.T("  модули, токены, роли, заявки не тронуты"))
	return nil
}

// installConfigPath — config.yaml установки с программами в bin: по
// указателю ochan.home рядом с ними, иначе по OCTOCHAN_HOME или
// ~/.octochan; OCTOCHAN_CONFIG сильнее.
func installConfigPath(bin string) string {
	if env := os.Getenv("OCTOCHAN_CONFIG"); env != "" {
		return env
	}
	if home, ok, err := workspace.ReadHomeFile(bin); err == nil && ok {
		return filepath.Join(home, "config.yaml")
	}
	if home := os.Getenv(workspace.EnvHome); home != "" {
		return filepath.Join(home, "config.yaml")
	}
	return filepath.Join(defaultHome(), "config.yaml")
}

// reportConfig — что стало со значениями прежней версии после первого
// запуска новой: новая переносит переименованные настройки сама и прежних
// строк не удаляет. Значение, которого в конфиге больше нет, — внимание:
// копия прежнего конфига — в архиве.
func reportConfig(out io.Writer, config string, before []byte, archive string) {
	if len(before) == 0 {
		return
	}
	after, err := os.ReadFile(config)
	if err != nil {
		fmt.Fprintln(out, i18n.Sprintf("  внимание: %s не прочитать после обновления: %v; прежний — в %s", config, err, archive))
		return
	}
	if bytes.Equal(before, after) {
		fmt.Fprintln(out, i18n.T("  config.yaml не изменился"))
		return
	}
	old, now := flattenYAML(string(before)), flattenYAML(string(after))
	var lost []string
	for key, value := range old {
		if got, ok := now[key]; !ok || fmt.Sprint(got) != fmt.Sprint(value) {
			lost = append(lost, key)
		}
	}
	sort.Strings(lost)
	if len(lost) == 0 {
		fmt.Fprintln(out, i18n.T("  config.yaml дополнен новой версией; все прежние значения на месте"))
		return
	}
	fmt.Fprintln(out, i18n.Sprintf("  внимание: в config.yaml после обновления нет прежних значений: %s — прежний конфиг — в %s",
		strings.Join(lost, ", "), archive))
}

// writeFileAtomic пишет файл через временный рядом: оборванная запись не
// оставит половину конфига.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".restore"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func init() {
	updateCmd.Flags().BoolVar(&updateRollback, "rollback", false, "вернуть программы из архива прежней версии")
	updateCmd.Flags().BoolVar(&updateList, "list", false, "показать архивы прежних версий")
	rootCmd.AddCommand(updateCmd)
}
