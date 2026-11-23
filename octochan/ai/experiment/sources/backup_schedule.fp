goal: backup

classes:
  Backup   : "backup"
  Schedule : "schedule"
  Database : "database"
  Storage  : "storage"

relations:
  Schedule --defines--> Database (nightly_backup)
  Database --strives_to--> Backup

cell c1:
  set Schedule.period = daily
  set Storage.quota = 500 GB