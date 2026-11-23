goal: stable

classes:
  Scaling : "scale"
  Load    : "load"
  Count   : "count"
  State   : "stable"

relations:
  Load --defines--> Scaling (load_threshold)
  Count --limits--> Scaling (replica_count)
  Scaling --strives_to--> State

cell c1:
  set Load.threshold = 70 %
  set Count.maximum = 10
