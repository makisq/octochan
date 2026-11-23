goal: available

classes:
  Deletion : "delete"
  Age      : "old"
  Storage  : "disk"
  State    : "available"

relations:
  Age --defines--> Deletion (old_limit)
  Deletion --modifies--> Storage (delete_size)
  Storage --strives_to--> State

cell c1:
  set Age.period = 7 d
  set Storage.threshold = 70..90 %
