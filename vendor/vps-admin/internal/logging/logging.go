package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// Setup настраивает стандартный логгер на запись одновременно в файл и stdout.
//
// stdout перехватывается systemd в journal, файл нужен для локального просмотра
// (`tail -f`) и хранения истории независимо от journald.
//
// Возвращает io.Closer для закрытия файла при завершении работы.
func Setup(logPath string) (io.Closer, error) {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	if logPath == "" {
		log.SetOutput(os.Stdout)
		return nopCloser{}, nil
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		// Не фатально — продолжаем писать только в stdout.
		log.SetOutput(os.Stdout)
		return nopCloser{}, err
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.SetOutput(os.Stdout)
		return nopCloser{}, err
	}

	log.SetOutput(io.MultiWriter(os.Stdout, f))
	return f, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
