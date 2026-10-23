package rbac

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Cost — стоимость хеширования argon2id. Значения по умолчанию — второй
// рекомендованный набор RFC 9106: три прохода по 64 МиБ.
type Cost struct {
	Time    uint32
	Memory  uint32 // КиБ
	Threads uint8
}

// HashCost — стоимость для новых хешей. Уже сохранённый хеш проверяется
// со своей стоимостью, поэтому её можно поднять, не ломая старые роли.
var HashCost = Cost{Time: 3, Memory: 64 * 1024, Threads: 2}

const (
	saltSize = 16
	keySize  = 32
)

// Secret — хеш логина или пароля с солью и стоимостью. Открытым текстом
// ни логин, ни пароль не хранятся нигде.
type Secret struct {
	Salt []byte
	Hash []byte
	Cost Cost
}

// NewSecret хеширует значение со свежей солью.
func NewSecret(value string) (*Secret, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("не удалось получить случайную соль: %w", err)
	}
	cost := HashCost
	return &Secret{Salt: salt, Hash: derive(value, salt, cost), Cost: cost}, nil
}

// Matches сравнивает значение с хешем за постоянное время.
func (s *Secret) Matches(value string) bool {
	if s == nil || len(s.Hash) != keySize {
		return false
	}
	return subtle.ConstantTimeCompare(derive(value, s.Salt, s.Cost), s.Hash) == 1
}

func derive(value string, salt []byte, cost Cost) []byte {
	return argon2.IDKey([]byte(value), salt, cost.Time, cost.Memory, cost.Threads, keySize)
}
