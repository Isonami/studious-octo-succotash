import * as React from 'react';
import {useCallback, useEffect, useState} from 'react';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Title from './Title';
import {useSnackbar} from "notistack";
import {cancelSync, getSyncs, SyncItem} from "./Api";
import {styled} from "@mui/material/styles";
import {LinearProgress, linearProgressClasses} from "@mui/material";
import byteSize from "byte-size";
import IconButton from "@mui/material/IconButton";
import CancelIcon from "@mui/icons-material/Cancel";

export type SyncProps = {
    cb: (n: number) => void;
};

const BorderLinearProgress = styled(LinearProgress)(({theme}) => ({
    height: 20,
    borderRadius: 5,
    [`&.${linearProgressClasses.colorPrimary}`]: {
        backgroundColor: theme.palette.grey[theme.palette.mode === 'light' ? 200 : 800],
    },
    [`& .${linearProgressClasses.bar}`]: {
        borderRadius: 5,
        backgroundColor: theme.palette.mode === 'light' ? '#1a90ff' : '#308fe8',
    },
}));

function toStringFn(this: { value: number, unit: string }): string {
    return `${this.value}${this.unit}`
}

export default function Sync(props: SyncProps) {
    const {enqueueSnackbar} = useSnackbar();
    const [syncs, setSyncs] = useState<[] | SyncItem[]>([]);

    const handleCancel = async (path: string) => {
        const result = await cancelSync(path)
        if (!result.ok) {
            enqueueSnackbar(result.error);
        }
    };

    const handleGetSyncs = useCallback(async () => {
        const result = await getSyncs()
        if (!result.ok) {
            enqueueSnackbar(result.error, {preventDuplicate: true});
        }

        return result.results
    }, [enqueueSnackbar]);

    useEffect(() => {
        const interval = setInterval(async () => {
            const syncs = await handleGetSyncs();
            setSyncs(syncs);
            props.cb(syncs.length);
        }, 2000);
        return () => {
            clearInterval(interval);
        };
    }, [handleGetSyncs, props]);

    return (
        <React.Fragment>
            <Title>Syncs</Title>
            <Table size="small">
                <TableHead>
                    <TableRow>
                        <TableCell>Path</TableCell>
                        <TableCell>Progress</TableCell>
                        <TableCell>Time Left</TableCell>
                        <TableCell>Transferred</TableCell>
                        <TableCell>Speed</TableCell>
                        <TableCell align="right"></TableCell>
                    </TableRow>
                </TableHead>
                <TableBody>
                    {syncs.map((sync) => (
                        <TableRow key={sync.path}>
                            <TableCell>{sync.path}</TableCell>
                            <TableCell><BorderLinearProgress variant="determinate" value={sync.progress}/></TableCell>
                            <TableCell>{sync.time_left}</TableCell>
                            <TableCell>{`${byteSize(sync.downloaded, {units: "iec", toStringFn})}`}</TableCell>
                            <TableCell>{`${byteSize(sync.speed, {units: "iec", toStringFn})}/s`}</TableCell>
                            <TableCell align="right"><IconButton aria-label="cancel" color="error"
                                                                 onClick={() => handleCancel(sync.path)}>
                                <CancelIcon/>
                            </IconButton></TableCell>
                        </TableRow>
                    ))}
                </TableBody>
            </Table>
        </React.Fragment>
    );
}
