import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { Dog } from 'lucide-react';
import Layout from './components/Layout';
import SetupWizard from './pages/Setup';
import Now from './pages/Now';
import Trail from './pages/Trail';
import History from './pages/History';
import Models from './pages/Models';
import Sessions from './pages/Sessions';
import SessionDetail from './pages/SessionDetail';
import Compactions from './pages/Compactions';
import Leaks from './pages/Leaks';
import Debug from './pages/Debug';
import Settings from './pages/Settings';
import { useDaemonHealth } from './hooks/useDaemonHealth';

export default function App() {
  const daemon = useDaemonHealth();

  if (daemon.health === 'unknown') return <Splash />;
  if (daemon.health === 'down') return <SetupWizard daemon={daemon} />;

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Now />} />
          <Route path="trail" element={<Trail />} />
          <Route path="history" element={<History />} />
          <Route path="models" element={<Models />} />
          <Route path="sessions" element={<Sessions />} />
          <Route path="sessions/:uuid" element={<SessionDetail />} />
          <Route path="compactions" element={<Compactions />} />
          <Route path="leaks" element={<Leaks />} />
          <Route path="debug" element={<Debug />} />
          <Route path="settings" element={<Settings />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}

function Splash() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-zinc-50 dark:bg-zinc-950">
      <Dog className="size-6 text-rose-500/60 animate-pulse" />
    </div>
  );
}
