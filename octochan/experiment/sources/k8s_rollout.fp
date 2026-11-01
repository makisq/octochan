goal: release

classes:
  Deployment : "deploy"
  Strategy   : "rolling"
  Batch      : "batch"
  Version    : "release"

relations:
  Strategy --defines--> Deployment (rolling_update)
  Batch --limits--> Deployment (batch_size)
  Deployment --strives_to--> Version

cell c1:
  set Strategy.maximum = 25 %
  set Batch.size = 1
