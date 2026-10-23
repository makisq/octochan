package rbac

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Допуск сервера.
//
// Сервер выполняет команду клиента отдельным процессом ochan в роли
// клиента. Задать роль процессу окружением можно только для роли без
// логина и пароля — иначе OCTOCHAN_ROLE=master обходил бы пароль master.
// Но клиент сервера логин и пароль уже предъявил, и сервер их проверил.
// Это он и подтверждает допуском: роль, срок и подпись ключом сервера.
//
// Ключ лежит в хранилище ролей и меняется при каждом запуске сервера,
// поэтому допуск не переживает сервер, а срок ограничивает его временем
// одной команды. От владельца сервера допуск не защищает и не должен:
// на своей машине он master. Он закрывает обход пароля по ошибке
// и случайно подсмотренный допуск после перезапуска.

// RotateServerKey задаёт новый ключ подписи допусков.
func (s *Store) RotateServerKey() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("не удалось получить ключ сервера: %w", err)
	}
	s.ServerKey = key
	return nil
}

// IssueGrant выдаёт допуск на роль до момента until.
func (s *Store) IssueGrant(role string, until time.Time) (string, error) {
	if len(s.ServerKey) == 0 {
		return "", errors.New("у хранилища нет ключа сервера")
	}
	payload := role + "|" + strconv.FormatInt(until.Unix(), 10)
	return payload + "|" + sign(s.ServerKey, payload), nil
}

// CheckGrant проверяет допуск на роль.
func (s *Store) CheckGrant(role, grant string, now time.Time) error {
	if len(s.ServerKey) == 0 {
		return errors.New("сервер не запускался: допусков нет")
	}
	parts := strings.Split(grant, "|")
	if len(parts) != 3 {
		return errors.New("допуск испорчен")
	}
	payload := parts[0] + "|" + parts[1]
	if !hmac.Equal([]byte(sign(s.ServerKey, payload)), []byte(parts[2])) {
		return errors.New("подпись допуска не сходится: его выдал не этот сервер или сервер перезапущен")
	}
	if !strings.EqualFold(parts[0], role) {
		return fmt.Errorf("допуск выдан на роль %s, а не %s", parts[0], role)
	}
	until, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("допуск испорчен")
	}
	if now.Unix() > until {
		return errors.New("срок допуска истёк")
	}
	return nil
}

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
