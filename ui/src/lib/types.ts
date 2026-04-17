// TypeScript interfaces matching the Go API JSON response shapes.
// Field names use the exact JSON keys from internal/api/*.go structs.

export interface QueueSlot {
	nzo_id: string;
	filename: string;
	name: string;
	category: string;
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

export interface HistorySlot {
	nzo_id: string;
	name: string;
	status: string;
	category: string;
	script: string;
	fail_message: string;
	storage: string;
	size: string;
	bytes: number;
	completed: number;
	script_log: string;
	script_line: string;
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
