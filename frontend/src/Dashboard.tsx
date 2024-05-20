import * as React from 'react';
import {useState} from 'react';
import {createTheme, styled, ThemeProvider} from '@mui/material/styles';
import CssBaseline from '@mui/material/CssBaseline';
import Box from '@mui/material/Box';
import MuiAppBar, {AppBarProps as MuiAppBarProps} from '@mui/material/AppBar';
import Toolbar from '@mui/material/Toolbar';
import Typography from '@mui/material/Typography';
import Container from '@mui/material/Container';
import Grid from '@mui/material/Grid';
import Paper from '@mui/material/Paper';
import DirsList from './DirsList';
import Sync from "./Sync";
import {SnackbarProvider} from 'notistack';

interface AppBarProps extends MuiAppBarProps {
    open?: boolean;
}

const AppBar = styled(MuiAppBar, {
    shouldForwardProp: (prop) => prop !== 'open',
})<AppBarProps>(({theme, open}) => ({
    zIndex: theme.zIndex.drawer + 1,
}));

const defaultTheme = createTheme({palette: {mode: "dark"}});


export default function Dashboard() {
    const [syncListSize, setSyncListSize] = useState<number>(0);

    const handleSyncCb = (syncListSize: number) => {
        setSyncListSize(syncListSize);
    };

    return (
        <ThemeProvider theme={defaultTheme}>
            <SnackbarProvider>
                <Box sx={{display: 'flex'}}>
                    <CssBaseline/>
                    <AppBar position="absolute">
                        <Toolbar>
                            <Typography
                                component="h1"
                                variant="h6"
                                color="inherit"
                                noWrap
                                sx={{flexGrow: 1}}
                            >
                                Syncer
                            </Typography>
                        </Toolbar>
                    </AppBar>
                    <Box
                        component="main"
                        sx={{
                            backgroundColor: (theme) =>
                                theme.palette.mode === 'light'
                                    ? theme.palette.grey[100]
                                    : theme.palette.grey[800],
                            flexGrow: 1,
                            marginTop: '64px',
                            height: 'calc(100vh - 64px)',
                            overflow: 'auto',
                        }}
                    >
                        <Container maxWidth="lg" sx={{mt: 4, mb: 4}}>

                            <Grid container spacing={3}>
                                <Grid item xs={12}>
                                    <Paper sx={{p: 2, display: 'flex', flexDirection: 'column'}}>
                                        <Sync cb={handleSyncCb}/>
                                    </Paper>
                                </Grid>
                                <Grid item xs={12}>
                                    <Paper sx={{p: 2, display: 'flex', flexDirection: 'column'}}>
                                        <DirsList syncListSize={syncListSize}/>
                                    </Paper>
                                </Grid>
                            </Grid>
                        </Container>
                    </Box>
                </Box>
            </SnackbarProvider>
        </ThemeProvider>
    );
}
