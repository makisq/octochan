goal: reload

classes:
  Reload : "reload"
  Config : "config"
  Node   : "node"
  Group  : "web"

relations:
  Config --modifies--> Node (config_change)
  Node   --strives_to--> Reload

hard forbid write(except = {web_config})

cell c1:
  set Config.version = 42
  set Group.size = 10