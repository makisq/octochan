goal: stable

classes:
  Memory  : "memory"
  Storage : "disk"
  Level   : "level"
  State   : "stable"

relations:
  Level --defines--> Memory (memory_level)
  Storage --limits--> Memory (disk_size)
  Memory --strives_to--> State

cell c1:
  set Level.level = 10
  set Storage.size = 4 GB
