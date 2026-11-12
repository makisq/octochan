package cmd

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Значок программы: запись в меню приложений и ярлык на рабочем столе —
// ochan находится поиском системы и запускается щелчком, в своей оболочке.
//
// Картинка — эмблема логотипа (tools/icongen): под Linux ochan несёт её в
// себе и кладёт рядом с записью меню, под Windows она встроена в ochan.exe
// ресурсом, и ярлык берёт её оттуда.

//go:embed icon.png
var iconPNG []byte

// desktopName — имя записи меню и ярлыка.
const desktopName = "ochan"

// installDesktop ставит значок программы program: запись в меню приложений
// и ярлык на рабочем столе; workdir — каталог, в котором ярлык её запускает.
func installDesktop(out io.Writer, program, workdir string) error {
	var made []string
	var err error
	switch runtime.GOOS {
	case "windows":
		made, err = windowsShortcuts(program, workdir, nil)
	case "linux", "freebsd", "openbsd", "netbsd":
		made, err = desktopEntries(program, workdir)
	default:
		return fmt.Errorf("значок ставится под Linux и Windows; под %s его пока нет — программа запускается из терминала", runtime.GOOS)
	}
	for _, path := range made {
		fmt.Fprintf(out, "Значок: %s\n", path)
	}
	return err
}

// dataHome — каталог данных пользователя по правилам freedesktop.
func dataHome() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// desktopDir — рабочий стол пользователя; пусто — его нет (сервер без
// графики).
func desktopDir() string {
	if out, err := exec.Command("xdg-user-dir", "DESKTOP").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); filepath.IsAbs(dir) {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				return dir
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Desktop")
	// Домашний каталог сам рабочим столом не считается: так отвечает
	// xdg-user-dir там, где стола нет.
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// desktopQuote — аргумент строки Exec записи меню: в кавычках, особые
// знаки — с обратной чертой.
func desktopQuote(arg string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
		case '%':
			b.WriteByte('%')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// desktopEntries — запись меню приложений freedesktop, картинка рядом и
// копия записи на рабочем столе.
func desktopEntries(program, workdir string) ([]string, error) {
	data, err := dataHome()
	if err != nil {
		return nil, err
	}
	icon := filepath.Join(data, "icons", "hicolor", "256x256", "apps", desktopName+".png")
	entry := filepath.Join(data, "applications", desktopName+".desktop")
	for _, dir := range []string{filepath.Dir(icon), filepath.Dir(entry)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("значок: %w", err)
		}
	}
	if err := os.WriteFile(icon, iconPNG, 0o644); err != nil {
		return nil, fmt.Errorf("значок: %w", err)
	}
	text := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Version=1.0\n" +
		"Name=ochan\n" +
		"GenericName=Configuration orchestrator\n" +
		"GenericName[ru]=Оркестратор конфигураций\n" +
		"Comment=ochan shell: compare, plan and apply configurations\n" +
		"Comment[ru]=Оболочка ochan: сравнение, планы и применение конфигураций\n" +
		"Exec=" + desktopQuote(program) + " shell\n" +
		"Path=" + workdir + "\n" +
		"Icon=" + icon + "\n" +
		"Terminal=true\n" +
		"Categories=System;Utility;\n" +
		"Keywords=ochan;octochan;окточан;\n"
	if err := os.WriteFile(entry, []byte(text), 0o644); err != nil {
		return nil, fmt.Errorf("значок: %w", err)
	}
	made := []string{entry}
	if desk := desktopDir(); desk != "" {
		// На рабочем столе запись должна быть исполняемой, иначе среда её
		// не запустит.
		path := filepath.Join(desk, desktopName+".desktop")
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			return made, fmt.Errorf("значок на рабочем столе: %w", err)
		}
		made = append(made, path)
	}
	return made, nil
}

// windowsShortcuts — ярлыки ochan.lnk в меню «Пуск» и на рабочем столе;
// dirs — каталоги вместо них. Ярлык создаёт оболочка Windows (WScript.Shell):
// формат .lnk — её, и писать его самим незачем. Значок ярлыка — из ochan.exe.
func windowsShortcuts(program, workdir string, dirs []string) ([]string, error) {
	script := `$ErrorActionPreference = 'Stop'
$dirs = @($env:OCHAN_SHORTCUT_DIRS -split '\|' | Where-Object { $_ })
if ($dirs.Count -eq 0) { $dirs = @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop')) }
$shell = New-Object -ComObject WScript.Shell
foreach ($dir in $dirs) {
    if (-not $dir -or -not (Test-Path -LiteralPath $dir)) { continue }
    $path = Join-Path $dir ($env:OCHAN_SHORTCUT_NAME + '.lnk')
    $link = $shell.CreateShortcut($path)
    $link.TargetPath = $env:OCHAN_SHORTCUT_EXE
    $link.Arguments = 'shell'
    $link.WorkingDirectory = $env:OCHAN_SHORTCUT_WORKDIR
    $link.IconLocation = $env:OCHAN_SHORTCUT_EXE + ',0'
    $link.Description = 'ochan'
    $link.Save()
    [Console]::Out.WriteLine($path)
}
`
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	c.Env = append(os.Environ(), "OCHAN_SHORTCUT_EXE="+program, "OCHAN_SHORTCUT_WORKDIR="+workdir,
		"OCHAN_SHORTCUT_NAME="+desktopName, "OCHAN_SHORTCUT_DIRS="+strings.Join(dirs, "|"))
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	var made []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			made = append(made, line)
		}
	}
	if err != nil {
		return made, fmt.Errorf("ярлык не создан: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(made) == 0 {
		return nil, errors.New("ярлык не создан: ни меню «Пуск», ни рабочего стола у пользователя нет")
	}
	return made, nil
}

// removeDesktop убирает значок, поставленный installDesktop: чего нет — не
// ошибка.
func removeDesktop(out io.Writer) error {
	var paths []string
	switch runtime.GOOS {
	case "windows":
		script := `foreach ($dir in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
    if ($dir) { [Console]::Out.WriteLine((Join-Path $dir ($env:OCHAN_SHORTCUT_NAME + '.lnk'))) }
}
`
		c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		c.Env = append(os.Environ(), "OCHAN_SHORTCUT_NAME="+desktopName)
		list, err := c.Output()
		if err != nil {
			return fmt.Errorf("ярлыки: %w", err)
		}
		for _, line := range strings.Split(string(list), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				paths = append(paths, line)
			}
		}
	default:
		data, err := dataHome()
		if err != nil {
			return err
		}
		paths = []string{filepath.Join(data, "applications", desktopName+".desktop"),
			filepath.Join(data, "icons", "hicolor", "256x256", "apps", desktopName+".png")}
		if desk := desktopDir(); desk != "" {
			paths = append(paths, filepath.Join(desk, desktopName+".desktop"))
		}
	}
	for _, path := range paths {
		switch err := os.Remove(path); {
		case err == nil:
			fmt.Fprintf(out, "Убран: %s\n", path)
		case !os.IsNotExist(err):
			return err
		}
	}
	return nil
}
