package pluginhost

import (
	"encoding/base64"
	"encoding/hex"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Конвертация cobra <-> protobuf была продублирована в core и в dwd, причём
// обе копии определяли тип флага через fmt.Sprintf("%T", flag.Value) и получали
// строку вида "*pflag.int32Value". Дальше её разбирали через strings.Contains,
// где случай "int" стоял раньше "int32"/"int64" и перехватывал их.
// pflag сам сообщает каноническое имя типа через Value.Type() — используем его.

// CommandsToProto переводит список команд cobra в protobuf-представление.
func CommandsToProto(cmds []*cobra.Command) []*Command {
	out := make([]*Command, 0, len(cmds))
	for _, cmd := range cmds {
		out = append(out, CommandToProto(cmd))
	}
	return out
}

// CommandToProto переводит команду cobra в protobuf-представление вместе
// с флагами и подкомандами.
func CommandToProto(cmd *cobra.Command) *Command {
	annotations := make(map[string]string, len(cmd.Annotations))
	for k, v := range cmd.Annotations {
		annotations[k] = v
	}

	return &Command{
		Use:                       cmd.Use,
		Short:                     cmd.Short,
		Long:                      cmd.Long,
		Example:                   cmd.Example,
		Aliases:                   cmd.Aliases,
		ValidArgs:                 cmd.ValidArgs,
		ValidArgsFunction:         cmd.ValidArgsFunction != nil,
		Args:                      cmd.Args != nil,
		ArgAliases:                cmd.ArgAliases,
		BashCompletionFunction:    cmd.BashCompletionFunction,
		Deprecated:                cmd.Deprecated,
		Hidden:                    cmd.Hidden,
		Annotations:               annotations,
		Version:                   cmd.Version,
		SilenceErrors:             cmd.SilenceErrors,
		SilenceUsage:              cmd.SilenceUsage,
		TraverseChildren:          cmd.TraverseChildren,
		FparseErrWhitelist:        &FParseErrWhitelist{UnknownFlags: cmd.FParseErrWhitelist.UnknownFlags},
		PersistentFlagSet:         cmd.HasPersistentFlags(),
		LocalFlagSet:              cmd.HasLocalFlags(),
		Flags:                     flagsToProto(cmd.Flags()),
		PersistentFlags:           flagsToProto(cmd.PersistentFlags()),
		RunFunction:               cmd.Run != nil || cmd.RunE != nil,
		PreRunFunction:            cmd.PreRun != nil || cmd.PreRunE != nil,
		PostRunFunction:           cmd.PostRun != nil || cmd.PostRunE != nil,
		PersistentPreRunFunction:  cmd.PersistentPreRun != nil || cmd.PersistentPreRunE != nil,
		PersistentPostRunFunction: cmd.PersistentPostRun != nil || cmd.PersistentPostRunE != nil,
		Commands:                  CommandsToProto(cmd.Commands()),
	}
}

func flagsToProto(fs *pflag.FlagSet) []*Flag {
	if fs == nil {
		return nil
	}

	var flags []*Flag
	fs.VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, &Flag{
			Name:      flag.Name,
			Shorthand: flag.Shorthand,
			Usage:     flag.Usage,
			DefValue:  flag.DefValue,
			Changed:   flag.Changed,
			Type:      flag.Value.Type(),
		})
	})
	return flags
}

// applyFlags регистрирует флаги из protobuf в наборе fs.
func applyFlags(fs *pflag.FlagSet, flags []*Flag) {
	for _, f := range flags {
		if f == nil || f.Name == "" {
			continue
		}
		registerFlag(fs, f)
	}
}

func registerFlag(fs *pflag.FlagSet, f *Flag) {
	name, short, usage, def := f.Name, f.Shorthand, f.Usage, f.DefValue

	// Ошибка разбора значения по умолчанию не должна ронять загрузку модуля,
	// но и молчать о ней нельзя.
	warn := func(kind string, err error) {
		log.Printf("модуль: флаг %q — некорректное значение по умолчанию для типа %s (%q): %v",
			name, kind, def, err)
	}

	switch f.Type {
	case "bool":
		v, err := strconv.ParseBool(def)
		if err != nil && def != "" {
			warn("bool", err)
		}
		fs.BoolP(name, short, v, usage)

	case "int":
		v, err := strconv.Atoi(def)
		if err != nil && def != "" {
			warn("int", err)
		}
		fs.IntP(name, short, v, usage)

	case "int8", "int16", "int32":
		v, err := strconv.ParseInt(def, 10, 32)
		if err != nil && def != "" {
			warn(f.Type, err)
		}
		fs.Int32P(name, short, int32(v), usage)

	case "int64":
		v, err := strconv.ParseInt(def, 10, 64)
		if err != nil && def != "" {
			warn("int64", err)
		}
		fs.Int64P(name, short, v, usage)

	case "uint", "uint8", "uint16", "uint32":
		v, err := strconv.ParseUint(def, 10, 32)
		if err != nil && def != "" {
			warn(f.Type, err)
		}
		fs.Uint32P(name, short, uint32(v), usage)

	case "uint64":
		v, err := strconv.ParseUint(def, 10, 64)
		if err != nil && def != "" {
			warn("uint64", err)
		}
		fs.Uint64P(name, short, v, usage)

	case "float32":
		v, err := strconv.ParseFloat(def, 32)
		if err != nil && def != "" {
			warn("float32", err)
		}
		fs.Float32P(name, short, float32(v), usage)

	case "float64":
		v, err := strconv.ParseFloat(def, 64)
		if err != nil && def != "" {
			warn("float64", err)
		}
		fs.Float64P(name, short, v, usage)

	case "duration":
		var v time.Duration
		if def != "" {
			parsed, err := time.ParseDuration(def)
			if err != nil {
				warn("duration", err)
			}
			v = parsed
		}
		fs.DurationP(name, short, v, usage)

	case "stringSlice":
		fs.StringSliceP(name, short, splitDefaultList(def), usage)

	case "stringArray":
		fs.StringArrayP(name, short, splitDefaultList(def), usage)

	case "intSlice":
		var values []int
		for _, part := range splitDefaultList(def) {
			n, err := strconv.Atoi(part)
			if err != nil {
				warn("intSlice", err)
				continue
			}
			values = append(values, n)
		}
		fs.IntSliceP(name, short, values, usage)

	case "boolSlice":
		var values []bool
		for _, part := range splitDefaultList(def) {
			b, err := strconv.ParseBool(part)
			if err != nil {
				warn("boolSlice", err)
				continue
			}
			values = append(values, b)
		}
		fs.BoolSliceP(name, short, values, usage)

	case "count":
		fs.CountP(name, short, usage)

	case "bytesHex":
		// Раньше значение декодировалось из base64, а регистрировалось как hex.
		v, err := hex.DecodeString(def)
		if err != nil && def != "" {
			warn("bytesHex", err)
			v = nil
		}
		fs.BytesHexP(name, short, v, usage)

	case "bytesBase64":
		v, err := base64.StdEncoding.DecodeString(def)
		if err != nil && def != "" {
			warn("bytesBase64", err)
			v = nil
		}
		fs.BytesBase64P(name, short, v, usage)

	case "string":
		fs.StringP(name, short, def, usage)

	default:
		// Неизвестный тип отдаём строкой: команда останется вызываемой.
		log.Printf("модуль: неизвестный тип флага %q у %q, регистрирую как string", f.Type, name)
		fs.StringP(name, short, def, usage)
	}
}

func splitDefaultList(def string) []string {
	def = strings.TrimSpace(def)
	def = strings.TrimPrefix(def, "[")
	def = strings.TrimSuffix(def, "]")
	if def == "" {
		return nil
	}

	parts := strings.Split(def, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// commandName возвращает имя команды без описания аргументов:
// из "diff <file1> <file2>" получается "diff".
func commandName(use string) string {
	fields := strings.Fields(use)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
