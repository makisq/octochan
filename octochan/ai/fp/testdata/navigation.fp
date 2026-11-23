# Тот же пример, исправленный так, чтобы проверка проходила:
# слот называется obstacle, а операции и отношения даёт скилл.
goal: northwest

classes:
  Direction   : "northwest"
  Movement    : "move"
  Trajectory  : "path"
  Obstacle    : "obstacle"
  Fuel        : "fuel"
  Speed       : "speed"

relations:
  Obstacle   --forces-->     Trajectory   (detour)
  Trajectory --defines-->    Fuel         (consumption)
  Fuel       --terminates--> Movement     (exhaustion)
  Movement   --strives_to--> Direction

priority:
  Fuel  <  Direction

generalization detour(obstacle : Obstacle, side : {left, right}) :
  Trajectory = path_around(obstacle, side)

instance detour_1 = detour(obstacle := o_17, side := right)

requires: fuel_consumption_model

import @driving v2.3
  provides: Obstacle, Trajectory, path_around, fuel_consumption_model, stop
  requires_class: Movement

??n_stop(except: {fuel_exhausted})

cell c1:
  set Speed.average = 30 km/h

cell c2:
  set detour.side = right
  binds_to: src/nav/path.c:40-88

cell c3:
  set detour.side.margin = 2 m

report:
  q: fuel_policy
    choice: stop_before_detour
    alternatives: {stop_before_detour, detour_anyway}
    basis: priority(Fuel < Direction)
