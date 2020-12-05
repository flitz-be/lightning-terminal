import React, { useEffect, useRef } from 'react';
import * as d3 from 'd3';
import { BatchChartData, ChartDimensions } from './chartUtils';

interface Props {
  data: BatchChartData[];
  dimensions: ChartDimensions;
  xScale: d3.ScaleBand<string>;
}

const BottomAxis: React.FC<Props> = ({ data, dimensions, xScale }) => {
  const axisRef = useRef<SVGGElement>(null);
  const { width, height } = dimensions;

  // const scale = d3
  //   .scaleBand()
  //   .range([0, width])
  //   .domain(data.map(b => b.id))
  //   .padding(0.4);

  useEffect(() => {
    if (!axisRef.current) return;
    // add the ticks and labels to the bottom axis
    d3.select(axisRef.current).call(d3.axisBottom(xScale));
    console.log('D3Chart: bottom axis scale', xScale.range());
  }, [data, dimensions]);

  return (
    <g
      ref={axisRef}
      className="axis-bottom"
      transform={`translate(0, ${height})`}
      style={{ fontSize: 14 }}
    />
  );
};

export default BottomAxis;
