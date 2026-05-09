// TypeScript interfaces matching the Go API JSON response shapes.
// Field names use the exact JSON keys from internal/api/*.go structs.

export interface QueueSlot {
	nzo_id: string;
	filename: string;
	name: string;
	cat: string;
	priority: string;
	status: string;
	script: string;
	password: string;
	size: string;
	sizeleft: string;
	mb: number;
	mbleft: number;
	bytes: number;
	remaining_bytes: number;
	percentage: string;
	pp: string;
	warning?: string;
	failed_bytes: number;
	par2_bytes: number;
	par2_files: number;
	current_stage: string;
	articles_remaining: number;
	eta_seconds: number;
	current_file: string;
}

export interface QueueDetail {
	status: string;
	paused: boolean;
	noofslots: number;
	noofslots_total: number;
	limit: number;
	start: number;
	slots: QueueSlot[];
}

export interface QueueResponse {
	status: boolean;
	queue: QueueDetail;
}

export interface HistoryStageLog {
	name: string;
	actions: string[];
}

export interface HistorySlot {
	nzo_id: string;
	name: string;
	nzb_name: string;
	status: string;
	category: string;
	script: string;
	fail_message: string;
	storage: string;
	path: string;
	size: string;
	bytes: number;
	downloaded: number;
	completeness: number;
	download_time: number;
	postproc_time: number;
	completed: number;
	stage_log: HistoryStageLog[];
	script_log: string;
	script_line: string;
	meta: string;
	url_info: string;
}

export interface HistoryDetail {
	noofslots: number;
	total_size: string;
	slots: HistorySlot[];
}

export interface HistoryResponse {
	status: boolean;
	history: HistoryDetail;
}

export interface WarningsResponse {
	status: boolean;
	warnings: string[];
}

export interface StatusResponse {
	status: boolean;
	error?: string;
}

export interface VersionResponse {
	status: boolean;
	version: string;
}

export interface ServerConfig {
	name: string;
	host: string;
	port: number;
	username: string;
	password: string;
	connections: number;
	ssl: boolean;
	ssl_verify: number;
	ssl_ciphers: string;
	priority: number;
	required: boolean;
	optional: boolean;
	retention: number;
	timeout: number;
	pipelining_requests: number;
	enable: boolean;
}

export interface CategoryConfig {
	name: string;
	pp: number;
	script: string;
	priority: number;
	dir: string;
	order: number;
}

export interface SorterConfig {
	name: string;
	order: number;
	min_size: number;
	multipart_label: string;
	sort_string: string;
	sort_cats: string[];
	sort_type: number[];
	is_active: boolean;
}

export interface ScheduleConfig {
	name: string;
	enabled: boolean;
	action: string;
	arguments: string;
	minute: string;
	hour: string;
	dayofweek: string;
}

export interface RSSFilterConfig {
	name: string;
	enabled: boolean;
	title: string;
	body: string;
	cat: string;
	pp: string;
	script: string;
	priority: number;
	type: string;
	size_from: number;
	size_to: number;
	age: number;
}

export interface RSSFeedConfig {
	name: string;
	uri: string;
	cat: string;
	pp: string;
	script: string;
	enable: boolean;
	priority: number;
	filters: RSSFilterConfig[];
}

export interface FullConfig {
	general: Record<string, any>;
	downloads: Record<string, any>;
	postproc: Record<string, any>;
	servers: ServerConfig[];
	categories: CategoryConfig[];
	sorters: SorterConfig[];
	schedules: ScheduleConfig[];
	rss: RSSFeedConfig[];
}

export interface ConfigResponse {
	status: boolean;
	config: FullConfig;
}

// Server status debug panel types
export interface ConnSnapshot {
	index: number;
	article_id: string;
	subject: string;
	bytes: number;
	since_unix: number;
	connected: boolean;
}

export interface ServerSnapshot {
	name: string;
	host: string;
	port: number;
	ssl: boolean;
	priority: number;
	pipelining: number;
	max_connections: number;
	active_conns: number;
	active: boolean;
	enabled: boolean;
	optional: boolean;
	required: boolean;
	penalty_until: number;
	bps: number;
	total_bytes: number;
	connections: ConnSnapshot[];
}

export interface ServerStatsResponse {
	servers: ServerSnapshot[];
}
