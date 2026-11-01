goal: safe

classes:
  Backup   : "backup"
  Storage  : "storage"
  Schedule : "schedule"
  Safety   : "safe"

relations:
  Schedule --defines--> Backup (backup_schedule)
  Storage --limits--> Backup (storage_quota)
  Backup --strives_to--> Safety

cell c1:
  set Schedule.period = daily
  set Storage.quota = 500 GB
