goal: safe

classes:
  Backup   : "snapshot"
  Storage  : "volume"
  Schedule : "schedule"
  Safety   : "safe"

relations:
  Schedule --defines--> Backup (snapshot_schedule)
  Storage --limits--> Backup (volume_quota)
  Backup --strives_to--> Safety

hard forbid write(except = {backup_volume})

cell c1:
  set Schedule.rate = weekly
  set Storage.count = 4
