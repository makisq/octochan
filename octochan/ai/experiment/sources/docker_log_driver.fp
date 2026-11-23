goal: available

classes:
  Log     : "journal"
  Storage : "volume"
  Limit   : "cap"
  State   : "available"

relations:
  Limit --limits--> Log (size_cap)
  Log --modifies--> Storage (journal_size)
  Storage --strives_to--> State

cell c1:
  set Limit.size = 100 MB
  set Log.size = 50..200 MB
  set Log.count = 3
