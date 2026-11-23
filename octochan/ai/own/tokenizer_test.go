package own

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// trainedVocabulary — словарь, на котором учились веса: 1679 токенов
// базового алфавита с формами (sha256 строк через \n).
const trainedVocabulary = "54b009fe589bc4c7cc76b7f8f2f54130f55f379493a92ee414553c6d6b251663"

func testAlphabet(t *testing.T) []AlphabetEntry {
	t.Helper()
	data, err := os.ReadFile("testdata/alphabet.tsv")
	if err != nil {
		t.Fatal(err)
	}
	return ParseAlphabet(string(data))
}

// Словарь и токены совпадают с прежним словарём модели (fpmtok.py): веса,
// обученные на нём, читаются без переобучения. testdata/fpmtok-cases.json —
// его выгрузка на задачах корпуса и краевых случаях: binds_to, больше 64
// имён, не ASCII, нечётный отступ.
func TestTokenizerMatchesTrainedVocabulary(t *testing.T) {
	data, err := os.ReadFile("testdata/fpmtok-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Cases []struct {
			Task    string   `json:"task"`
			Answer  *string  `json:"answer"`
			IDs     []int    `json:"ids"`
			Start   int      `json:"start"`
			Names   []string `json:"names"`
			Decoded string   `json:"decoded"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	tok, err := BuildTokenizer(testAlphabet(t), true)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Join(tok.Tokens, "\n")))
	if got := hex.EncodeToString(sum[:]); len(tok.Tokens) != 1679 || got != trainedVocabulary {
		t.Fatalf("словарь: %d токенов, sha256 %s", len(tok.Tokens), got)
	}
	for i, c := range want.Cases {
		ids, start, names, err := tok.Pair(c.Task, c.Answer)
		if err != nil {
			t.Fatalf("случай %d: %v", i, err)
		}
		if !reflect.DeepEqual(ids, c.IDs) || start != c.Start || !reflect.DeepEqual(names.List(), c.Names) {
			t.Fatalf("случай %d:\n%s\nтокены %v\nожидались %v\nимена %v / %v", i, c.Task, ids, c.IDs, names.List(), c.Names)
		}
		if c.Answer != nil {
			_, _, taskNames, _ := tok.Pair(c.Task, nil)
			if got := tok.Decode(ids[start:len(ids)-1], taskNames.Copy()); got != c.Decoded {
				t.Fatalf("случай %d: декодирование\n%q\nожидалось\n%q", i, got, c.Decoded)
			}
		}
	}
}

// Новое имя — следующая свободная ячейка cK.
func TestNamesFreshCell(t *testing.T) {
	n := NewNames("db7", "c1")
	if got := n.Name(2); got != "c2" || n.Len() != 3 {
		t.Fatalf("новое имя %q, имён %d", got, n.Len())
	}
	if n.Name(9) != "" {
		t.Fatal("переменная дальше следующей — не имя")
	}
}

func TestTokenizerSaveLoad(t *testing.T) {
	tok, err := BuildTokenizer(testAlphabet(t), true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tok.Save(dir); err != nil {
		t.Fatal(err)
	}
	back, err := LoadTokenizer(dir)
	if err != nil || !reflect.DeepEqual(back.Tokens, tok.Tokens) || back.Eos != tok.Eos {
		t.Fatalf("словарь после записи: %v", err)
	}
}
