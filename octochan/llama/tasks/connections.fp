# Поднять лимит соединений, не трогая память.
goal: tuned

classes:
  State  : "tuned"
  Config : "config"
  Limit  : "limit", "connections"
  Memory : "memory"

relations:
  Limit  --modifies-->   Config (connection_limit)
  Memory --limits-->     Limit  (memory_budget)
  Config --strives_to--> State

priority:
  Memory < Limit     # бюджет памяти важнее числа соединений

# Граница того, что модель подставит в set_param(c, name, value): значение —
# от 100 до 500. Слот limit типизирован классом: слово «connections» в нём
# означает Limit.
generalization connection_limit(target : Config, limit : Limit, value : 100..500) :
  Config = set_param(target, limit, value)

import @pgconf v1
  provides: Config, set_param, store
  requires_class: Config

requires: store

hard forbid store(except = {prod_conf})

cell c1:
  set Memory.work_mem = 4 MB
