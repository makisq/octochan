//go:build !unix

package workspace

// checkPrivate — на Windows права каталога задают списки доступа, а не биты
// режима: ochan их не разбирает, и замечаний о каталоге там нет.
func checkPrivate(string, bool) (string, error) { return "", nil }
