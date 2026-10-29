package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"octochan/workspace"

	"github.com/spf13/cobra"
)

// Переменные окружения, которыми сервер пары primary/standby говорит с
// дочерним процессом: узел и эпоха — для отметки «кто выполняет», адрес
// и ключ барьера — чтобы отметка дошла до резерва до начала выполнения.
const (
	EnvHANode    = "OCTOCHAN_HA_NODE"
	EnvHABarrier = "OCTOCHAN_HA_BARRIER"
	// EnvHAStandby — процесс резервного узла: журнал аудита у него свой,
	// общий он получает копией от primary.
	EnvHAStandby = "OCTOCHAN_HA_STANDBY"
)

// beginRun отмечает начало выполнения заявки. На сервере с резервом
// отметка сначала доходит до резерва: если primary пропадёт посреди
// выполнения, новый primary увидит, что заявка выполнялась, и не выполнит
// её второй раз — исход будет неизвестен, а не удвоен.
func beginRun(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) error {
	host, _ := os.Hostname()
	mark := workspace.RunnerMark{Host: host, PID: os.Getpid()}
	if node, epoch, ok := strings.Cut(os.Getenv(EnvHANode), ":"); ok {
		mark.Node = node
		mark.Epoch, _ = strconv.ParseInt(epoch, 10, 64)
	}
	if err := store.MarkRunning(request, mark); err != nil {
		return err
	}
	if err := haBarrier(cmd); err != nil {
		if uerr := store.Unrun(request); uerr != nil {
			cmd.PrintErrf("внимание: заявку %s не удалось вернуть в «согласована»: %v\n", request.ID, uerr)
		}
		return fmt.Errorf("выполнение заявки %s не начато: %w", request.ID, err)
	}
	return nil
}

// settleRunning разбирает заявку, которая числится выполняемой. Процесс
// на этой машине жив — ждать итога. Пропал — исход неизвестен: заявка
// упала, её пересобирают. На другой машине живость не проверить — ждать
// или переключение пары серверов решит (ochan ha promote).
func settleRunning(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) error {
	mark := request.Runner
	if mark == nil {
		return fmt.Errorf("заявка %s выполняется, а кем — не записано", request.ID)
	}
	host, _ := os.Hostname()
	if mark.Host != host || processAlive(mark.PID) {
		return fmt.Errorf("заявка %s уже выполняется: %s — дождитесь итога", request.ID, mark)
	}
	err := fmt.Errorf("исход неизвестен: процесс %d на %s пропал, не записав итога выполнения", mark.PID, mark.Host)
	if ferr := store.MarkFailed(request, request.AppliedRef, err); ferr != nil {
		return ferr
	}
	explainFailure(cmd, store, request)
	return err
}

// haBarrier — отметка дошла до резерва. Вне пары серверов — сразу.
func haBarrier(cmd *cobra.Command) error {
	spec := os.Getenv(EnvHABarrier)
	if spec == "" {
		return nil
	}
	url, token, ok := strings.Cut(spec, " ")
	if !ok {
		return fmt.Errorf("%s испорчен", EnvHABarrier)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Barrier "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("сервер не ответил на барьер резерва: %w", err)
	}
	defer resp.Body.Close()
	var reply struct {
		Error   string `json:"error"`
		Warning string `json:"warning"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	if resp.StatusCode >= 300 {
		if reply.Error == "" {
			reply.Error = resp.Status
		}
		return fmt.Errorf("резервный сервер не подтвердил копию: %s", reply.Error)
	}
	if reply.Warning != "" {
		cmd.PrintErrf("внимание: %s\n", reply.Warning)
	}
	return nil
}
