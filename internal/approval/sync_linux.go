package approval

import "os"

func syncJournal(f *os.File) error { return f.Sync() }
