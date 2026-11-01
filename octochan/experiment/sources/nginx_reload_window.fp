goal: reload

classes:
  Reload : "reload"
  Config : "config"
  Period : "window"
  Group  : "pool"

relations:
  Period --defines--> Reload (reload_window)
  Group --strives_to--> Reload

cell c1:
  set Period.window = night
  set Group.size = 12
