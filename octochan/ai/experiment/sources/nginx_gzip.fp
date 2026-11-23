goal: fast

classes:
  Speed  : "throughput"
  Config : "config"
  Load   : "traffic"
  Pace   : "fast"

relations:
  Load --modifies--> Config (traffic_change)
  Config --defines--> Speed (config_speed)
  Speed --strives_to--> Pace

cell c1:
  set Load.minimum = 1 KB
  set Config.level = 5
