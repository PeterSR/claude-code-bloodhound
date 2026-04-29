import { BrowserRouter, Routes, Route } from 'react-router-dom';
import Layout from './components/Layout';
import Now from './pages/Now';
import History from './pages/History';
import Sessions from './pages/Sessions';
import SessionDetail from './pages/SessionDetail';
import Compactions from './pages/Compactions';
import Leaks from './pages/Leaks';
import Debug from './pages/Debug';
import Settings from './pages/Settings';

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Now />} />
          <Route path="history" element={<History />} />
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
