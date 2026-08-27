package onexit

func Do(f func()) {
	lock.Lock()
	defer lock.Unlock()

	funcs = append(funcs, f)
}
