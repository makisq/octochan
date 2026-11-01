goal: safe

classes:
  Drain  : "drain"
  Node   : "node"
  Period : "window"
  Safety : "safe"

relations:
  Period --defines--> Drain (maintenance_window)
  Drain --modifies--> Node (node_drain)
  Node --strives_to--> Safety

hard forbid write(except = {node_label})

cell c1:
  set Period.window = night
  set Drain.timeout = 30 s
