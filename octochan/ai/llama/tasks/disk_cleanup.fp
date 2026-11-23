goal: available

classes:
  State   : "available"
  Storage : "disk"
  Log     : "log"
  Backup  : "backup"

relations:
  Log     --modifies--> Storage (log_size)
  Storage --strives_to--> State

hard forbid write(except = {old_log})

cell c1:
  set Storage.threshold = 80 %
  set Backup.count = 7