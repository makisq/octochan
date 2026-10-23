package lang

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"octochan/config"

	"github.com/spf13/viper"
)

// execTry выполняет тело и передаёт ошибку в catch.
//
// Управляющие сигналы (break, continue, return) ошибками не являются
// и сквозь catch проходят насквозь: перехватывать выход из цикла или
// возврат из алиаса было бы сюрпризом.
func (in *Interpreter) execTry(node *Try, env *scope) error {
	err := in.execStmt(node.Body, newScope(env))
	if err == nil {
		return nil
	}
	if isControlSignal(err) {
		return err
	}

	catchScope := newScope(env)
	if node.ErrVar != "" {
		catchScope.define(node.ErrVar, TypeString, StringValue(errorMessage(err)))
	}
	return in.execStmt(node.Catch, catchScope)
}

// isControlSignal отличает выход из цикла и возврат от настоящей ошибки.
func isControlSignal(err error) bool {
	if errors.Is(err, errBreak) || errors.Is(err, errContinue) {
		return true
	}
	var ret returnSignal
	return errors.As(err, &ret)
}

// errorMessage отдаёт текст ошибки без позиции и стека: в сценарии
// обычно нужен смысл сбоя, а не место в исходнике.
func errorMessage(err error) string {
	var runtimeErr *RuntimeError
	if errors.As(err, &runtimeErr) {
		return runtimeErr.Msg
	}
	return err.Error()
}

// execForParallel выполняет итерации одновременно.
//
// Основной сценарий предметной области — «сделать это с N серверами»,
// и делать их по очереди странно. Но интерпретатор не потокобезопасен,
// поэтому каждая итерация получает собственный вычислитель со своим стеком
// вызовов и своим буфером вывода.
func (in *Interpreter) execForParallel(node *For, env *scope, items []Value) error {
	if len(items) == 0 {
		return nil
	}

	limit := viper.GetInt("defaults.max_parallel_tasks")
	if limit <= 0 {
		limit = 5
	}
	if limit > len(items) {
		limit = len(items)
	}

	// Снимок окружения: тело видит копию внешних переменных, а его
	// присваивания наружу не выходят. Иначе две итерации писали бы
	// в одну map одновременно — это гонка на уровне Go, а не только
	// путаница в семантике.
	base := env.snapshot()

	outputs := make([]bytes.Buffer, len(items))
	errOutputs := make([]bytes.Buffer, len(items))
	errs := make([]error, len(items))
	workers := make([]*Interpreter, len(items))

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, limit)

	for i, item := range items {
		wg.Add(1)
		go func(idx int, item Value) {
			defer wg.Done()

			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-in.ctx.Done():
				errs[idx] = in.ctx.Err()
				return
			}

			// Паника в одной итерации не должна ронять весь процесс
			// и терять результаты остальных.
			defer func() {
				if r := recover(); r != nil {
					errs[idx] = fmt.Errorf("паника в итерации %d: %v", idx+1, r)
				}
			}()

			worker := in.fork(&outputs[idx], &errOutputs[idx])
			workers[idx] = worker

			iterScope := base.snapshot()
			iterScope.define(node.Variable, node.Declared, item)

			if node.Declared != "" {
				if err := checkType(node.Declared,
					fmt.Sprintf("элемент цикла %s", node.Variable), item); err != nil {
					errs[idx] = worker.wrap(node.Pos, err)
					return
				}
			}

			errs[idx] = worker.execStmt(node.Body, iterScope)
		}(i, item)
	}

	wg.Wait()

	// Действия итераций — тоже в порядке элементов: план параллельного
	// цикла должен совпадать от запуска к запуску. Действия упавших
	// итераций остаются в списке: записанное до сбоя уже записано.
	for _, worker := range workers {
		if worker != nil {
			for _, effect := range worker.effects {
				// Итерация переписала файл: печать после цикла — новое действие.
				delete(in.appending, effect.Path)
			}
			in.effects = append(in.effects, worker.effects...)
			in.calls = append(in.calls, worker.calls...)
		}
	}

	// Вывод сбрасывается в порядке итераций, а не в порядке завершения:
	// результат параллельного цикла обязан быть воспроизводимым. Так же
	// дописывается и печать итераций в файлы.
	for i := range outputs {
		for _, stream := range []struct {
			buf *bytes.Buffer
			dst io.Writer
		}{{&outputs[i], in.out}, {&errOutputs[i], in.errOut}} {
			if stream.buf.Len() == 0 {
				continue
			}
			if _, err := io.Copy(stream.dst, stream.buf); err != nil {
				return in.errorf(node.Pos, "не удалось записать вывод итерации %d: %v", i+1, err)
			}
		}
	}
	if err := in.flushAppends(workers); err != nil {
		return in.errorf(node.Pos, "parallel for: %v", err)
	}

	return in.joinIterationErrors(node, items, errs)
}

// joinIterationErrors собирает отказы итераций в одну ошибку.
//
// break, continue и return внутри параллельного цикла запрещены: они
// означают управление последовательным ходом выполнения, которого здесь нет.
func (in *Interpreter) joinIterationErrors(node *For, items []Value, errs []error) error {
	var failures []string

	for i, err := range errs {
		if err == nil {
			continue
		}
		if isControlSignal(err) {
			return in.errorf(node.Pos,
				"break, continue и return недопустимы внутри parallel for: "+
					"итерации выполняются одновременно, и прерывать нечего")
		}

		// Многострочный текст ошибки выравниваем целиком, иначе вторая
		// и последующие строки уезжают к левому краю.
		message := strings.ReplaceAll(errorMessage(err), "\n", "\n    ")
		failures = append(failures, fmt.Sprintf("  %s: %s", Display(items[i]), message))
	}

	if len(failures) == 0 {
		return nil
	}

	// Сообщение говорит, сколько итераций из скольких упало: при пятидесяти
	// серверах это первое, что нужно знать.
	return in.errorf(node.Pos, "parallel for: неудачных итераций %d из %d:\n%s",
		len(failures), len(items), strings.Join(failures, "\n"))
}

// fork создаёт вычислитель для одной итерации.
//
// Реестр алиасов, набор встроенных функций и песочница во время выполнения
// только читаются, поэтому их можно разделять; записи режима плана общие,
// как общий диск, и защищены своей блокировкой. Стек вызовов, вывод
// и список действий у каждой итерации свои; печать в файлы итерация
// копит и отдаёт после цикла.
func (in *Interpreter) fork(out, errOut io.Writer) *Interpreter {
	return &Interpreter{
		registry:     in.registry,
		builtins:     in.builtins,
		out:          out,
		errOut:       errOut,
		deferAppends: true,
		ctx:          in.ctx,
		sandbox:      in.sandbox,
		dir:          in.dir,
		dry:          in.dry,
		callIDs:      append([]int{}, in.callIDs...),
		nextCall:     in.nextCall,
		stack:        append([]string{}, in.stack...),
	}
}

// snapshot делает плоскую копию видимых переменных.
//
// Контейнеры копируются вглубь: словарь и конфиг — ссылочные значения,
// и без копии put() из двух итераций писал бы в одну Go-map.
func (s *scope) snapshot() *scope {
	flat := newScope(nil)

	var chain []*scope
	for cur := s; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}

	// Идём от внешней области к внутренней, чтобы ближние переменные
	// затеняли дальние, как при обычном поиске.
	for i := len(chain) - 1; i >= 0; i-- {
		for name, value := range chain[i].vars {
			flat.vars[name] = copyValue(value)
		}
		for name, declared := range chain[i].types {
			flat.types[name] = declared
		}
		for name, inferred := range chain[i].inferred {
			flat.inferred[name] = inferred
		}
	}
	return flat
}

// copyValue делает глубокую копию изменяемых значений.
func copyValue(v Value) Value {
	switch val := v.(type) {
	case ListValue:
		out := make(ListValue, len(val))
		for i, item := range val {
			out[i] = copyValue(item)
		}
		return out

	case MapValue:
		out := make(MapValue, len(val))
		for key, item := range val {
			out[key] = copyValue(item)
		}
		return out

	case ConfigValue:
		data := make(config.ConfigMap, len(val.Data))
		for section, params := range val.Data {
			copied := make(map[string]string, len(params))
			for key, value := range params {
				copied[key] = value
			}
			data[section] = copied
		}
		return ConfigValue{Name: val.Name, Data: data, Notes: val.Notes}

	default:
		// Числа, строки и логические значения неизменяемы.
		return v
	}
}
