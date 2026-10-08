import { render, screen } from '@testing-library/react';
import { useEffect, useState } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { ServicesProvider, useServices } from './ServicesProvider';
import { createMockServices, sampleServerInfo } from './mock';

function VersionLabel() {
  const { info } = useServices();
  const [version, setVersion] = useState('…');
  useEffect(() => {
    void info.serverInfo().then((i) => {
      setVersion(i.version);
    });
  }, [info]);
  return <span>version: {version}</span>;
}

describe('ServicesProvider', () => {
  it('hands the mock services to components through useServices()', async () => {
    const services = createMockServices({ info: sampleServerInfo({ version: '9.9.9-test' }) });
    render(
      <ServicesProvider services={services}>
        <VersionLabel />
      </ServicesProvider>,
    );
    expect(await screen.findByText('version: 9.9.9-test')).toBeInTheDocument();
  });

  it('lets tests replace a single service with a spy', async () => {
    const services = createMockServices();
    const serverInfo = vi.fn().mockResolvedValue(sampleServerInfo({ version: 'spy' }));
    render(
      <ServicesProvider services={{ ...services, info: { ...services.info, serverInfo } }}>
        <VersionLabel />
      </ServicesProvider>,
    );
    expect(await screen.findByText('version: spy')).toBeInTheDocument();
    expect(serverInfo).toHaveBeenCalledTimes(1);
  });

  it('throws when used outside the provider', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => render(<VersionLabel />)).toThrow(/within a <ServicesProvider>/);
    spy.mockRestore();
  });
});
