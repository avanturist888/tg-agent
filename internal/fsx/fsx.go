// Package fsx — запись файлов данных, которая переживает сбой питания.
//
// os.WriteFile пишет поверх файла: оборванная запись оставляет половину.
// Временный файл + rename без fsync тоже не спасает: NTFS успевает записать
// переименование и размер, а данные — нет, и после жёсткой перезагрузки файл
// целиком из нулей (9.11.2026 так обнулились все три outbox.json, отправка
// стояла до утра). Здесь данные сбрасываются на диск до переименования:
// после сбоя на месте либо прежний файл, либо новый целиком.
package fsx

import (
	"os"
	"path/filepath"
	"time"
)

// WriteFile — атомарная замена path содержимым data.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, perm)
	// на Windows замена может упереться в читающего соседа — переждём
	for i := 0; ; i++ {
		err = os.Rename(tmp, path)
		if err == nil {
			ok = true
			return nil
		}
		if i == 40 {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Zeroed — файл не пуст и состоит только из нулевых байтов: след сбоя
// питания, а не чьих-то данных.
func Zeroed(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// Quarantine — отложить испорченный файл рядом (не удаляя) и вернуть новый путь.
func Quarantine(path string) (string, error) {
	dst := path + ".broken-" + time.Now().Format("20060102-150405")
	return dst, os.Rename(path, dst)
}
