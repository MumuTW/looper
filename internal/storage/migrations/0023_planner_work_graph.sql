-- A Planner-owned graph is the durable authority for child work ordering.
-- Nodes keep their worker-loop identity even while blocked, so queue delivery
-- can be replayed without creating a second implementation loop.
CREATE TABLE planner_work_graphs (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  parent_repo TEXT NOT NULL,
  parent_issue_number INTEGER NOT NULL,
  planner_loop_id TEXT NOT NULL,
  base_branch TEXT NOT NULL,
  status TEXT NOT NULL,
  replan_reason TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  FOREIGN KEY (planner_loop_id) REFERENCES loops (id) ON DELETE CASCADE,
  CHECK (parent_issue_number > 0),
  CHECK (status IN ('active', 'replan_required', 'completed')),
  UNIQUE (planner_loop_id)
);

CREATE TABLE planner_work_graph_nodes (
  graph_id TEXT NOT NULL,
  node_key TEXT NOT NULL,
  goal TEXT NOT NULL,
  acceptance_criteria_json TEXT NOT NULL,
  expected_pr_scope TEXT NOT NULL,
  worker_loop_id TEXT NOT NULL,
  branch TEXT NOT NULL,
  state TEXT NOT NULL,
  blocked_reason TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (graph_id, node_key),
  UNIQUE (worker_loop_id),
  FOREIGN KEY (graph_id) REFERENCES planner_work_graphs (id) ON DELETE CASCADE,
  FOREIGN KEY (worker_loop_id) REFERENCES loops (id) ON DELETE CASCADE,
  CHECK (state IN ('pending', 'queued', 'running', 'completed', 'failed', 'closed', 'invalid'))
);

CREATE TABLE planner_work_graph_dependencies (
  graph_id TEXT NOT NULL,
  node_key TEXT NOT NULL,
  depends_on_key TEXT NOT NULL,
  PRIMARY KEY (graph_id, node_key, depends_on_key),
  FOREIGN KEY (graph_id, node_key) REFERENCES planner_work_graph_nodes (graph_id, node_key) ON DELETE CASCADE,
  FOREIGN KEY (graph_id, depends_on_key) REFERENCES planner_work_graph_nodes (graph_id, node_key) ON DELETE CASCADE,
  CHECK (node_key <> depends_on_key)
);

CREATE INDEX idx_planner_work_graph_nodes_graph_state
  ON planner_work_graph_nodes (graph_id, state, node_key);
CREATE INDEX idx_planner_work_graph_dependencies_dependency
  ON planner_work_graph_dependencies (graph_id, depends_on_key, node_key);
