goal: available

classes:
  Storage      : "disk"
  Notification : "alert"
  Limit        : "threshold"
  State        : "available"

relations:
  Limit --defines--> Notification (threshold_alert)
  Storage --strives_to--> State

cell c1:
  set Limit.threshold = 85 %
  set Notification.threshold = 80..90 %
