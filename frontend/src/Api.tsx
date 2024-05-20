import axios, {AxiosResponse, HttpStatusCode} from "axios";

export type Dir = {
    path: string;
    synced: boolean;
};

export type SyncItem = {
    path: string;
    progress: number;
    speed: number;
    downloaded: number;
    time_left: string;
};

export type BaseResponse = {
    error?: string
};

export type BaseResult = {
    ok: boolean;
    error?: string;
};

export interface DirsResponse extends BaseResponse {
    results: Dir[];
}

export interface DirsResult extends BaseResult {
    results: Dir[];
}

export interface SyncsResponse extends BaseResponse {
    results: SyncItem[];
}

export interface SyncsResult extends BaseResult {
    results: SyncItem[];
}

function isError(response: AxiosResponse<BaseResponse>) {
    return !(response && response.status === HttpStatusCode.Ok);
}

function formatError(response: AxiosResponse<BaseResponse>): string {
    return `${response.status}: ${response.data && response.data.error ? response.data.error : response.statusText}`
}

export async function getDirs(): Promise<DirsResult> {
    const url = "/api/dirs";
    try {
        const response = await axios.get<DirsResponse>(url, {validateStatus: () => true});
        return isError(response) ? {
            ok: false,
            error: formatError(response),
            results: [],
        } : {
            ok: true,
            results: response.data.results ? response.data.results : [],
        }
    } catch (error: any) {
        return {
            ok: false,
            error: error.toString(),
            results: [],
        }
    }
}

export async function getSyncs(): Promise<SyncsResult> {
    const url = "/api/syncs";
    try {
        const response = await axios.get<SyncsResponse>(url, {validateStatus: () => true});
        return isError(response) ? {
            ok: false,
            error: formatError(response),
            results: [],
        } : {
            ok: true,
            results: response.data.results ? response.data.results : [],
        }
    } catch (error: any) {
        return {
            ok: false,
            error: error.toString(),
            results: [],
        }
    }
}

export async function syncDir(path: string): Promise<BaseResult> {
    const url = "/api/sync";
    try {


        const response = await axios.post<BaseResponse>(url, {path: path}, {validateStatus: () => true})
        return isError(response) ? {
            ok: false,
            error: formatError(response),
        } : {
            ok: true,
        }
    } catch (error: any) {
        return {
            ok: false,
            error: error.toString(),
        }
    }
}

export async function cancelSync(path: string): Promise<BaseResult> {
    const url = "/api/cancel";
    try {
        const response = await axios.post<BaseResponse>(url, {path: path}, {validateStatus: () => true})
        return isError(response) ? {
            ok: false,
            error: formatError(response),
        } : {
            ok: true,
        }
    } catch (error: any) {
        return {
            ok: false,
            error: error.toString(),
        }
    }
}
