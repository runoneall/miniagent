package kvstore

func kvSet(key, value string) error {
	lock.Lock()
	defer lock.Unlock()

	store[key] = value
	return save()
}
