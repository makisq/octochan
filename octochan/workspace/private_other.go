//go:build !unix

package workspace

// checkPrivate — на Windows права каталога задают списки доступа, а не биты
// режима: ochan их не разбирает, и вынесенный каталог там не проверяется.
func checkPrivate(string) error { return nil }
