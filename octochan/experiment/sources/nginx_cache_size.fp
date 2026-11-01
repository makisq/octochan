goal: fast

classes:
  Cache   : "cache"
  Storage : "disk"
  Size    : "size"
  Pace    : "fast"

relations:
  Storage --limits--> Cache (disk_quota)
  Size --defines--> Cache (cache_size)
  Cache --strives_to--> Pace

cell c1:
  set Storage.maximum = 10 GB
  set Size.size = 100 MB
