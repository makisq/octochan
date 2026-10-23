# Пример из описания синтаксиса, без правок
goal: northwest

classes:
  Goal        : "northwest"
  Movement    : "move"
  Trajectory  : "path"
  Obstacle    : "obstacle"
  Fuel        : "fuel"
  Speed       : "speed"

relations:
  Obstacle   --forces-->     Trajectory   (detour)
  Trajectory --defines-->    Fuel         (consumption)
  Fuel       --terminates--> Movement     (exhaustion)
  Movement   --strives_to--> Goal

priority:
  Fuel  <  Goal      # исчерпание топлива важнее достижения цели
  Speed <  Fuel

generalization detour(obstacle : Obstacle, side : {left, right}) :
  Trajectory = path_around(obstacle, side)

instance detour_1 = detour(o := o_17, side := right)

requires: fuel_consumption_model   # ⊂ K, иначе ⊥undef

import @driving v2.3
  provides: Obstacle, Trajectory
  requires_class: Movement

hard forbid disconnect(except = {maintenance_window})
hard require verify_config(scope = web_group)

cell c3:
  set Trajectory.detour_side = right
  binds_to: src/nav/path.c:40-88

report:
  q: detour_side
    choice: right
    alternatives: {left, right}
    basis: not_in_prompt
  q: fuel_policy
    choice: stop_before_detour
    basis: priority(Fuel < Goal)
