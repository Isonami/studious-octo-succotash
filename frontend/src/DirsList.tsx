import * as React from 'react';
import {useCallback, useEffect, useState} from 'react';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Title from './Title';
import {styled} from '@mui/material/styles';
import DownloadIcon from '@mui/icons-material/Download';
import SyncIcon from '@mui/icons-material/Sync';
import IconButton from "@mui/material/IconButton";
import {useSnackbar} from 'notistack';
import {Dir, getDirs, syncDir} from "./Api";

export type DirsListProps = {
    syncListSize: number;
};

const DirRow = styled(TableRow)<{ ownerState: boolean }>(({theme, ownerState}) => (ownerState ? {
    backgroundColor: theme.palette.success.main,
} : {}));

export default function DirsList(props: DirsListProps) {
    const {enqueueSnackbar} = useSnackbar();
    const [dirs, setDirs] = useState<[] | Dir[]>([]);

    const handleSync = async (path: string) => {
        const result = await syncDir(path)
        if (!result.ok) {
            enqueueSnackbar(result.error);
        }
    };

    const handleGetDirs = useCallback(async () => {
        const result = await getDirs()
        if (!result.ok) {
            enqueueSnackbar(result.error);
        }

        return result.results
    }, [enqueueSnackbar]);

    useEffect(() => {
        (async () => {
            const dirs = await handleGetDirs();
            setDirs(dirs);
        })();
    }, [handleGetDirs, props.syncListSize]);


    return (
        <React.Fragment>
            <Title>Remote dirs</Title>
            <Table size="small">
                <TableHead>
                    <TableRow>
                        <TableCell>Path</TableCell>
                        <TableCell>Synced</TableCell>
                        <TableCell align="right"></TableCell>
                    </TableRow>
                </TableHead>
                <TableBody>
                    {dirs.map((row) => (
                        <DirRow key={row.path} ownerState={row.synced}>
                            <TableCell>{row.path}</TableCell>
                            <TableCell>{row.synced ? "yes" : "no"}</TableCell>
                            <TableCell align="right">{row.synced ?
                                <IconButton aria-label="resync" color="default" onClick={() => handleSync(row.path)}>
                                    <SyncIcon/>
                                </IconButton> :
                                <IconButton aria-label="sync" color="success" onClick={() => handleSync(row.path)}>
                                    <DownloadIcon/>
                                </IconButton>}</TableCell>
                        </DirRow>
                    ))}
                </TableBody>
            </Table>
        </React.Fragment>
    );
}
