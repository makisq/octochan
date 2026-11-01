package workspace

import (
	"strings"
	"testing"
	"time"

	"ochan/config"
)

func newRequest(summary string) *Request {
	return &Request{
		Source:  "restart.yaml",
		Service: "pangolin_restart",
		Summary: summary,
		Payload: "service: pangolin_restart\n",
		Plan: &config.Plan{Changes: []config.Change{
			{Kind: config.ChangeTask, Target: "CI-1", Name: "pangolin_restart"},
		}},
	}
}

func TestRequestLifecycle(t *testing.T) {
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	request := newRequest("операций: 1")
	if err := store.Create(request); err != nil {
		t.Fatal(err)
	}
	if request.State != RequestPending {
		t.Errorf("новая заявка в состоянии %q", request.State)
	}
	if request.ID == "" || request.Author == "" {
		t.Error("идентификатор или автор не проставлены")
	}

	// Выполнять несогласованную нельзя.
	if err := store.MarkApplied(request, []string{"1001"}); err == nil {
		t.Error("несогласованная заявка помечена выполненной")
	}

	if err := store.Decide(request, RequestApproved, "начальник", "ок"); err != nil {
		t.Fatal(err)
	}
	if request.State != RequestApproved || request.DecidedBy != "начальник" {
		t.Errorf("после согласования: %+v", request)
	}

	if err := store.MarkApplied(request, []string{"1001", "1002"}); err != nil {
		t.Fatal(err)
	}
	if request.State != RequestApplied || len(request.AppliedRef) != 2 {
		t.Errorf("после выполнения: %+v", request)
	}

	// Состояние должно пережить перечитывание с диска.
	reloaded, err := store.Get(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != RequestApplied || reloaded.DecidedBy != "начальник" {
		t.Errorf("состояние не сохранилось: %+v", reloaded)
	}
	if reloaded.Payload == "" {
		t.Error("текст сценария не сохранился: выполнять будет нечего")
	}
}

func TestRequestDecisionIsFinal(t *testing.T) {
	// Решение принимается один раз: пересмотр — это новая заявка.
	// Иначе журнал перестаёт отвечать, что именно было одобрено.
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	request := newRequest("x")
	if err := store.Create(request); err != nil {
		t.Fatal(err)
	}
	if err := store.Decide(request, RequestRejected, "начальник", "не сейчас"); err != nil {
		t.Fatal(err)
	}

	err = store.Decide(request, RequestApproved, "другой", "передумали")
	if err == nil {
		t.Fatal("отклонённая заявка согласована повторно")
	}
	if !strings.Contains(err.Error(), "один раз") {
		t.Errorf("непонятное сообщение: %v", err)
	}
	if request.State != RequestRejected {
		t.Errorf("состояние изменилось на %q", request.State)
	}
}

func TestRequestRejectsInvalidDecision(t *testing.T) {
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	request := newRequest("x")
	if err := store.Create(request); err != nil {
		t.Fatal(err)
	}
	if err := store.Decide(request, RequestApplied, "кто-то", ""); err == nil {
		t.Error("выполнение выдано за решение о согласовании")
	}
}

func TestRequestListNewestFirst(t *testing.T) {
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Время проставляется явно. Часы грубее, чем цикл создания: заявки,
	// созданные подряд, получают одну и ту же отметку, и тогда тест
	// проверял бы не сортировку, а разрешение системного таймера.
	base := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for i, summary := range []string{"первая", "вторая", "третья"} {
		request := newRequest(summary)
		request.Created = base.Add(time.Duration(i) * time.Minute)
		if err := store.Create(request); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("получено %d заявок", len(all))
	}

	got := []string{all[0].Summary, all[1].Summary, all[2].Summary}
	want := []string{"третья", "вторая", "первая"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("порядок %v, ожидался %v", got, want)
			break
		}
	}
}

func TestRequestListIsDeterministic(t *testing.T) {
	// Заявки с одинаковым временем создания обязаны выстраиваться
	// в один и тот же порядок при каждом чтении: это журнал согласований,
	// и «тот же набор, другой порядок» в нём недопустим.
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for _, summary := range []string{"первая", "вторая", "третья", "четвёртая"} {
		request := newRequest(summary)
		request.Created = created
		if err := store.Create(request); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("получено %d заявок", len(all))
	}

	// Второй ключ сортировки — идентификатор. Проверяется именно он,
	// а не «два чтения подряд совпали»: совпали бы они и со случайным
	// порядком, лишь бы файлы перечислялись одинаково.
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			ids := make([]string, len(all))
			for j, request := range all {
				ids[j] = request.ID
			}
			t.Fatalf("при равном времени порядок не задан идентификатором: %v", ids)
		}
	}
}

func TestRequestGetByPrefix(t *testing.T) {
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	request := newRequest("x")
	if err := store.Create(request); err != nil {
		t.Fatal(err)
	}

	found, err := store.Get(request.ID[:4])
	if err != nil {
		t.Fatalf("поиск по префиксу: %v", err)
	}
	if found.ID != request.ID {
		t.Errorf("найдена %s вместо %s", found.ID, request.ID)
	}

	if _, err := store.Get("нет-такой"); err == nil {
		t.Error("несуществующая заявка нашлась")
	}
}

func TestRequestEmptyStore(t *testing.T) {
	store, err := NewRequests(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	all, err := store.List()
	if err != nil {
		t.Fatalf("пустое хранилище вернуло ошибку: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("в пустом хранилище нашлось %d заявок", len(all))
	}
}
