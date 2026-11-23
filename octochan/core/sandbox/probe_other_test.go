//go:build !linux

package sandbox

// runPlatform: проб системных вызовов Linux на этой системе нет. Тесты,
// которые ими пользуются, пропускаются в requireIsolation ещё до запуска.
func runPlatform(probeReq) (bool, error) { return false, nil }
