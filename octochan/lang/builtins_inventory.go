package lang

import (
	"errors"
	"fmt"

	"octochan/inventory"
)

// errNoInventory — у сценария нет инвентаря.
var errNoInventory = errors.New("инвентаря нет: источники задаются в config.yaml (inventory.sources) " +
	"или файлом ~/.octochan/inventory.yaml")

func inventoryBuiltins() []Builtin {
	return []Builtin{
		{Name: "inventory", MinArgs: 0, MaxArgs: 1, Returns: TypeList,
			Doc: "хосты инвентаря словарями: host, groups и переменные; аргумент — селектор " +
				"(group=db, host=db*); с --limit — только подходящие под него",
			Fn: builtinInventory},
	}
}

func builtinInventory(in *Interpreter, args []Value) (Value, error) {
	if in.inventoryErr != nil {
		return nil, in.inventoryErr
	}
	if in.inventory == nil {
		return nil, errNoInventory
	}
	hosts := in.inventory
	if len(args) == 1 {
		sel, err := inventory.ParseSelector(args[0].Text())
		if err != nil {
			return nil, fmt.Errorf("inventory: %v", err)
		}
		hosts = inventory.Filter(hosts, []inventory.Selector{sel})
	}
	list := make(ListValue, 0, len(hosts))
	for _, h := range hosts {
		m := MapValue{}
		for key, value := range h.Vars {
			m[key] = fromPlain(value)
		}
		groups := make(ListValue, len(h.Groups))
		for i, g := range h.Groups {
			groups[i] = StringValue(g)
		}
		m["host"], m["groups"] = StringValue(h.Name), groups
		list = append(list, m)
	}
	return list, nil
}
