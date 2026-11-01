goal: running

classes:
  Upgrade : "upgrade"
  Restart : "reboot"
  Period  : "window"
  State   : "running"

relations:
  Period --defines--> Upgrade (maintenance_window)
  Upgrade --precedes--> Restart (upgrade_reboot)
  Restart --strives_to--> State

cell c1:
  set Period.window = weekend
  set Upgrade.version = 6
