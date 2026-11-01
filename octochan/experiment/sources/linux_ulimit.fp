goal: tuned

classes:
  File    : "file"
  Process : "process"
  Limit   : "limit"
  State   : "tuned"

relations:
  Limit --limits--> Process (file_limit)
  Process --strives_to--> State

cell c1:
  set Limit.maximum = 65536
  set File.count = 100000
